package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// listTablesSQL enumerates the relations whose contents pg_dump will write.
//
// Three exclusions matter, and each of them would otherwise produce a manifest that verification
// could never satisfy:
//
//   - Partitions are skipped and their parent is counted instead. Counting both would double
//     total_records, because a partitioned parent's count already includes every leaf.
//   - Foreign tables are skipped. pg_dump does not dump their data, so a count taken here could
//     never be reproduced in a restored copy.
//   - Materialized views are skipped. pg_dump emits a REFRESH rather than rows, so their contents
//     after a restore depend on whether the refresh ran, not on what the backup contained.
const listTablesSQL = `
	SELECT n.nspname, c.relname, pg_total_relation_size(c.oid)
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'p')
	  AND NOT c.relispartition
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND n.nspname NOT LIKE 'pg\_toast%'
	  AND n.nspname NOT LIKE 'pg\_temp%'
	  AND n.nspname NOT LIKE 'pg\_toast\_temp%'
	ORDER BY n.nspname, c.relname`

// tableRef is one relation to count.
type tableRef struct {
	schema string
	name   string
	size   int64
}

// qualified renders the relation as a safely quoted identifier for interpolation into a count.
func (t tableRef) qualified() string {
	return pgx.Identifier{t.schema, t.name}.Sanitize()
}

// objectName is how the relation appears in the manifest: schema-qualified, unquoted, so that the
// same string is produced on the restored copy regardless of quoting.
func (t tableRef) objectName() string {
	return t.schema + "." + t.name
}

// collectManifest records what the source contained, as exact per-table row counts.
//
// It must run inside the same repeatable-read transaction whose snapshot pg_dump is given, so the
// numbers describe precisely the rows the artifact holds. Counting outside that transaction against
// a live database produces a manifest that disagrees with the backup, and slice A5 would then read
// the disagreement as a failed verification — a false alarm on a perfectly good backup is the worst
// outcome this product can produce.
//
// Counts are exact, so is_sampled stays false. Sampling would go here, driven by a method option:
// estimate from pg_class.reltuples for relations above some size, set is_sampled = true, and
// VerifyRestore would then have to compare within a tolerance rather than for equality. That is a
// different verification semantic, not a faster version of this one, which is why it is not a
// silent fallback for large tables.
func collectManifest(ctx context.Context, q queryer, database string, capturedAt time.Time) (*fwv1.SourceManifest, error) {
	tables, err := listTables(ctx, q)
	if err != nil {
		return nil, err
	}

	manifest := &fwv1.SourceManifest{
		CapturedAt:   timestamppb.New(capturedAt),
		Entries:      make([]*fwv1.ManifestEntry, 0, len(tables)),
		TotalObjects: int64(len(tables)),
		IsSampled:    false,
	}

	for _, table := range tables {
		var count int64
		// The identifier is quoted by pgx.Identifier.Sanitize and comes from the catalog rather
		// than from a request, so it cannot carry an injection. A count cannot be parameterized:
		// a relation name is not a value.
		countSQL := "SELECT count(*) FROM " + table.qualified()
		if err := q.QueryRow(ctx, countSQL).Scan(&count); err != nil {
			return nil, sdk.Internal("count rows in %s", table.objectName()).WithCause(err)
		}

		manifest.Entries = append(manifest.Entries, &fwv1.ManifestEntry{
			Database:    database,
			ObjectName:  table.objectName(),
			RecordCount: count,
			SizeBytes:   table.size,
		})
		manifest.TotalRecords += count
	}

	return manifest, nil
}

// listTables reads the relations to count.
func listTables(ctx context.Context, q queryer) ([]tableRef, error) {
	rows, err := q.Query(ctx, listTablesSQL)
	if err != nil {
		return nil, sdk.Internal("list tables").WithCause(err)
	}
	defer rows.Close()

	var tables []tableRef
	for rows.Next() {
		var t tableRef
		if err := rows.Scan(&t.schema, &t.name, &t.size); err != nil {
			return nil, sdk.Internal("scan table row").WithCause(err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, sdk.Internal("list tables").WithCause(err)
	}
	return tables, nil
}

// exportSnapshot opens the consistent point both the manifest and pg_dump will see.
//
// The transaction must stay open until pg_dump has finished: a snapshot exported by a transaction
// that has ended can no longer be imported, and PostgreSQL would reject the dump's --snapshot.
func exportSnapshot(ctx context.Context, q queryer) (string, error) {
	var id string
	if err := q.QueryRow(ctx, `SELECT pg_export_snapshot()`).Scan(&id); err != nil {
		return "", sdk.Internal("export a consistent snapshot").WithCause(err)
	}
	if id == "" {
		return "", sdk.Internal("the server exported an empty snapshot identifier")
	}
	return id, nil
}
