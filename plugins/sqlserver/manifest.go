package sqlserver

import (
	"context"
	"database/sql"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// listTablesSQL enumerates the user tables whose rows the artifact holds.
//
// Two exclusions, and each of them would otherwise produce a manifest verification could never
// satisfy. Microsoft-shipped tables are skipped because they are the engine's own and a restored
// copy is free to differ. Views, temporal history tables' parents, and everything that is not a
// base table are outside sys.tables to begin with, which is why nothing else needs excluding here.
const listTablesSQL = `
	SELECT s.name, t.name,
	       ISNULL(SUM(CASE WHEN p.index_id IN (0, 1) THEN p.reserved_page_count ELSE 0 END) * 8192, 0)
	FROM sys.tables t
	JOIN sys.schemas s ON s.schema_id = t.schema_id
	LEFT JOIN sys.dm_db_partition_stats p ON p.object_id = t.object_id
	WHERE t.is_ms_shipped = 0
	GROUP BY s.name, t.name
	ORDER BY s.name, t.name`

// rowCountSnapshotSQL reads the row counts SQL Server maintains as metadata.
//
// It is not the manifest's number — maintained counts are approximate while a transaction is in
// flight — but it is free to read, which is exactly what a drift detector has to be. Comparing one
// taken before the backup with one taken after the counting pass answers the only question that
// matters: did anybody write to this object while we were looking at it.
const rowCountSnapshotSQL = `
	SELECT s.name, t.name, ISNULL(SUM(p.row_count), 0)
	FROM sys.tables t
	JOIN sys.schemas s ON s.schema_id = t.schema_id
	LEFT JOIN sys.dm_db_partition_stats p
	       ON p.object_id = t.object_id AND p.index_id IN (0, 1)
	WHERE t.is_ms_shipped = 0
	GROUP BY s.name, t.name`

// tableRef is one table to count.
type tableRef struct {
	schema string
	name   string
	size   int64
}

// collectManifest records what the source contained, as per-table row counts.
//
// The counts and the artifact cannot be pinned to one another the way PostgreSQL's can. pg_dump
// counts inside the very snapshot it exports; BACKUP DATABASE is consistent at the LSN it ends on,
// and the only ways to read a database as of that LSN — snapshot isolation, a database snapshot —
// are writes to a monitored instance, which Fleetward does not make.
//
// So the counting pass is bracketed by SQL Server's own maintained row counts, taken before the
// backup began and again after the counts are in hand. An object nobody wrote to in that window has
// an exact count and a mismatch on it is data loss. An object that changed is flagged, and a
// mismatch on it is drift — reported as INCONCLUSIVE rather than FAILED, because crying wolf on the
// one alert this product exists to raise costs more than a verification an operator has to look at
// (ADR-0022).
//
// On a quiescent database — a nightly window, and every case the conformance suite runs — nothing
// is flagged and FAILED still fires.
func collectManifest(
	ctx context.Context,
	db *sql.DB,
	database string,
	before map[string]int64,
	capturedAt time.Time,
) (*fwv1.SourceManifest, error) {
	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}

	manifest := &fwv1.SourceManifest{
		CapturedAt:   timestamppb.New(capturedAt),
		Entries:      make([]*fwv1.ManifestEntry, 0, len(tables)),
		TotalObjects: int64(len(tables)),
		// Every count below is a COUNT(*), never an estimate. Sampling is a different verification
		// semantic rather than a faster version of this one, so it is not a silent fallback.
		IsSampled: false,
	}

	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		// The identifier comes from sys.tables rather than from the request, and is bracket-quoted
		// exactly as QUOTENAME quotes it. A table name is not a value, so it cannot be a parameter.
		query := "SELECT COUNT_BIG(*) FROM " + qualified(table.schema, table.name)
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, sdk.Internal("count rows in %s", objectName(table.schema, table.name)).
				WithCause(classifyConnError(err))
		}
		counts[objectName(table.schema, table.name)] = count
	}

	after, err := rowCountSnapshot(ctx, db)
	if err != nil {
		return nil, err
	}

	for _, table := range tables {
		name := objectName(table.schema, table.name)
		count := counts[name]

		// Absent from either snapshot means the table was created or dropped inside the window,
		// which is the same answer as a changed count: this number cannot be tied to the artifact.
		beforeCount, seenBefore := before[name]
		afterCount, seenAfter := after[name]
		drifted := !seenBefore || !seenAfter || beforeCount != afterCount

		manifest.Entries = append(manifest.Entries, &fwv1.ManifestEntry{
			Database:            database,
			ObjectName:          name,
			RecordCount:         count,
			SizeBytes:           table.size,
			CountMayHaveDrifted: drifted,
		})
		manifest.TotalRecords += count
	}

	return manifest, nil
}

// listTables reads the tables to count.
func listTables(ctx context.Context, db *sql.DB) ([]tableRef, error) {
	rows, err := db.QueryContext(ctx, listTablesSQL)
	if err != nil {
		return nil, sdk.Internal("list tables").WithCause(classifyConnError(err))
	}
	defer func() { _ = rows.Close() }()

	var tables []tableRef
	for rows.Next() {
		var t tableRef
		if err := rows.Scan(&t.schema, &t.name, &t.size); err != nil {
			return nil, sdk.Internal("read a table row").WithCause(err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.Internal("list tables").WithCause(err)
	}
	return tables, nil
}

// rowCountSnapshot reads the maintained row counts, keyed the way the manifest names objects.
func rowCountSnapshot(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, rowCountSnapshotSQL)
	if err != nil {
		return nil, sdk.Internal("read the row-count snapshot").WithCause(classifyConnError(err))
	}
	defer func() { _ = rows.Close() }()

	snapshot := make(map[string]int64)
	for rows.Next() {
		var (
			schema string
			table  string
			count  int64
		)
		if err := rows.Scan(&schema, &table, &count); err != nil {
			return nil, sdk.Internal("read a row-count row").WithCause(err)
		}
		snapshot[objectName(schema, table)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.Internal("read the row-count snapshot").WithCause(err)
	}
	return snapshot, nil
}
