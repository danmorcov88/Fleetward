package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// historySource is what a report says it read. It is display only; core never branches on it.
	historySource = "msdb.dbo.backupset"

	// defaultHistoryLimit bounds a request that named no limit. msdb.dbo.backupset on an instance
	// that has been up for years, with nobody running sp_delete_backuphistory, holds a row per
	// database per backup per type since the day it was installed — hundreds of thousands of them.
	// A page is not an optimization here, it is the difference between a poll and an incident.
	defaultHistoryLimit = 500

	// maxHistoryLimit caps what a caller may ask for in one call, whatever it asks for.
	maxHistoryLimit = 5000

	// defaultHistoryTimeout bounds one poll. Reading history is background work against a
	// production server: it either answers promptly or it gets out of the way.
	defaultHistoryTimeout = 2 * time.Minute
)

// backupHistoryCapabilities is what this plugin promises about the evidence it reads.
//
// msdb.dbo.backupset is the richest observable backup history of any engine on the roadmap, and
// each field below is a statement about it that core acts on generically.
func backupHistoryCapabilities() *fwv1.BackupHistoryCapabilities {
	return &fwv1.BackupHistoryCapabilities{
		Supported:         true,
		SourceDescription: historySource,
		// backup_set_uuid is assigned by the engine when the backup set is written, and it does not
		// change if somebody moves the file it describes.
		IdentityIsEngineAssigned: true,
		// A row appears in backupset only when a backup completed, so a row is evidence of success
		// — which is exactly what a directory listing can never prove. A backup that failed writes
		// no row at all and therefore surfaces as a window nothing satisfied, which is the answer a
		// DBA wants. is_damaged carries the remaining case.
		ReportsOutcome: true,
		// It is read over the same connection every other RPC uses. Nothing on the filesystem is
		// involved.
		RequiresSharedDirectory: false,
	}
}

// ListBackupHistory reports what the instance itself recorded about backups taken on it, including
// backups nothing to do with Fleetward (ADR-0015).
//
// It is strictly read-only. Two SELECTs against msdb, no temporary objects, no writes, nothing
// created on the monitored instance.
func (p *Plugin) ListBackupHistory(ctx context.Context, req *fwv1.ListBackupHistoryRequest) (*fwv1.ListBackupHistoryResponse, error) {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultHistoryTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	limit := int(req.GetLimit())
	switch {
	case limit <= 0:
		limit = defaultHistoryLimit
	case limit > maxHistoryLimit:
		limit = maxHistoryLimit
	}

	// The continuation is the finish time the last page ended on, so a page boundary that falls in
	// the middle of a second cannot drop a row: the next page re-reads it, and core's upsert on the
	// backup set's own identity makes that harmless (ADR-0027).
	since := req.GetSince().AsTime()
	if token := req.GetPageToken(); token != "" {
		parsed, err := time.Parse(time.RFC3339Nano, token)
		if err != nil {
			return nil, sdk.InvalidArgument("the page token is not one this plugin issued")
		}
		since = parsed
	}
	if since.IsZero() {
		since = time.Unix(0, 0).UTC()
	}

	db, err := open(req.GetCredentials(), masterDatabase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	clock, err := serverClock(runCtx, db)
	if err != nil {
		return nil, err
	}

	rows, err := readBackupSets(runCtx, db, clock, since.UTC(), req.GetDatabases(), limit)
	if err != nil {
		return nil, err
	}

	resp := &fwv1.ListBackupHistoryResponse{Backups: rows}
	// A full page means the evidence may continue past it. An empty one never does.
	if len(rows) == limit && len(rows) > 0 {
		resp.NextPageToken = rows[len(rows)-1].GetFinishedAt().AsTime().Format(time.RFC3339Nano)
	}
	return resp, nil
}

// serverClock is how this instance's local time is turned into UTC.
//
// msdb.dbo.backupset stores backup_start_date and backup_finish_date as `datetime`, in the server's
// own local time, with no offset recorded anywhere. Comparing that to a UTC compliance window
// without knowing the offset produces adherence answers that are wrong by an hour twice a year, in
// the direction nobody checks — so the conversion is done here, deliberately, rather than left for
// something further up to assume.
//
// Naming the zone is what makes the conversion exact: AT TIME ZONE then applies the offset that was
// in force on the day of the backup, so a July backup read in January converts correctly. Where the
// zone cannot be named, all that is available is the offset in force right now, which is wrong by
// exactly one daylight-saving transition for any backup on the other side of one — and those
// records are marked approximate rather than silently trusted.
type serverClockInfo struct {
	// timezone is the identifier AT TIME ZONE accepts. Empty when this instance will not give one.
	timezone string
	// offset is the server's current offset from UTC. Used only when timezone is empty.
	offset time.Duration
}

func (c serverClockInfo) exact() bool { return c.timezone != "" }

// serverClock asks the instance what time zone it is in, and refuses to believe the answer until it
// has seen it work.
//
// The probe is not defensive programming, it is a measured finding. There are two functions here
// and only one of them returns something AT TIME ZONE will take:
//
//	CURRENT_TIMEZONE()     → "(UTC) Coordinated Universal Time"   ← a display name; rejected
//	CURRENT_TIMEZONE_ID()  → "UTC"                                ← the identifier
//
// CURRENT_TIMEZONE_ID arrived in SQL Server 2022, so an instance older than that cannot name its
// zone in a form anything can use, and there is no supported mapping from the display name to the
// identifier — the one that exists reads the Windows registry, which this plugin will not do. Those
// instances take the offset path and say so per record.
//
// Verifying the identifier against the engine rather than trusting it costs one cheap query and
// turns the whole class of "this instance names its zone in a way we cannot use" into a degraded
// answer instead of a failed poll.
func serverClock(ctx context.Context, db *sql.DB) (serverClockInfo, error) {
	var zone string
	if err := db.QueryRowContext(ctx, `SELECT CURRENT_TIMEZONE_ID()`).Scan(&zone); err == nil {
		if zone = strings.TrimSpace(zone); zone != "" && usableTimeZone(ctx, db, zone) {
			return serverClockInfo{timezone: zone}, nil
		}
	}

	var minutes int
	if err := db.QueryRowContext(ctx,
		`SELECT DATEDIFF(MINUTE, GETUTCDATE(), GETDATE())`).Scan(&minutes); err != nil {
		return serverClockInfo{}, classifyHistoryError(err)
	}
	return serverClockInfo{offset: time.Duration(minutes) * time.Minute}, nil
}

// usableTimeZone asks the engine to perform the conversion once, on a value it already has, before
// any of the real query depends on it.
func usableTimeZone(ctx context.Context, db *sql.DB, zone string) bool {
	var probe time.Time
	err := db.QueryRowContext(ctx,
		`SELECT CAST(SYSDATETIME() AT TIME ZONE @tz AS datetime2)`, sql.Named("tz", zone)).Scan(&probe)
	return err == nil
}

// readBackupSets runs the one query this RPC is made of.
//
// The filter on backup_finish_date is deliberately expressed in the server's own local time rather
// than by converting every row to UTC and comparing there. Converting the column would make the
// predicate non-sargable, and an index scan of a table with several hundred thousand rows on every
// poll is exactly what the limit exists to prevent. Any imprecision the boundary conversion
// introduces is under an hour, and core re-reads a window far wider than that on every poll.
func readBackupSets(
	ctx context.Context,
	db *sql.DB,
	clock serverClockInfo,
	since time.Time,
	databases []string,
	limit int,
) ([]*fwv1.ObservedBackup, error) {
	var (
		toUTC     func(column string) string
		sinceExpr string
		args      []any
	)
	if clock.exact() {
		args = append(args, sql.Named("tz", clock.timezone))
		toUTC = func(column string) string {
			return "CAST(" + column + " AT TIME ZONE @tz AT TIME ZONE 'UTC' AS datetime2)"
		}
		sinceExpr = "CAST(@since AT TIME ZONE 'UTC' AT TIME ZONE @tz AS datetime)"
	} else {
		// Both directions use the same current offset, so the row's stored value comes back
		// unchanged and Go applies the shift. Keeping the arithmetic in one place is what makes
		// finished_at_is_approximate mean exactly one thing.
		toUTC = func(column string) string { return "CAST(" + column + " AS datetime2)" }
		sinceExpr = "DATEADD(MINUTE, @offsetMinutes, @since)"
		args = append(args, sql.Named("offsetMinutes", int(clock.offset.Minutes())))
	}
	args = append(args, sql.Named("since", since), sql.Named("limit", limit))

	filter := ""
	if named := nonEmpty(databases); len(named) > 0 {
		placeholders := make([]string, 0, len(named))
		for i, name := range named {
			key := "db" + strconv.Itoa(i)
			placeholders = append(placeholders, "@"+key)
			args = append(args, sql.Named(key, name))
		}
		filter = " AND bs.database_name IN (" + strings.Join(placeholders, ", ") + ")"
	}

	// backup_set_uuid has been written since SQL Server 2005; backup_set_id is msdb's own identity
	// column and covers anything that predates it. Both are assigned by the engine, which is what
	// identity_is_engine_assigned promises.
	// Every interpolated fragment is chosen from this file rather than supplied by a caller: two
	// conversion expressions from serverClock, one boundary expression, and a list of placeholder
	// names this function generated. Every value — the time zone, the instant, the limit, each
	// database name — travels as a named parameter, which is what the placeholders are.
	//nolint:gosec // G201: the format arguments are literals from this function; values are parameters
	query := fmt.Sprintf(`
		SELECT TOP (@limit)
		       COALESCE(CONVERT(char(36), bs.backup_set_uuid),
		                CONCAT('backup_set_id:', CONVERT(varchar(20), bs.backup_set_id))),
		       bs.database_name,
		       bs.type,
		       %s,
		       %s,
		       COALESCE(bs.backup_size, 0),
		       COALESCE(bs.is_damaged, 0),
		       COALESCE(bs.has_backup_checksums, 0),
		       COALESCE(bs.user_name, ''),
		       COALESCE(bs.recovery_model, ''),
		       COALESCE(mf.physical_device_name, '')
		FROM   msdb.dbo.backupset AS bs
		LEFT   JOIN msdb.dbo.backupmediafamily AS mf
		       ON  mf.media_set_id = bs.media_set_id
		       AND mf.family_sequence_number = 1
		WHERE  bs.backup_finish_date IS NOT NULL
		  AND  bs.backup_finish_date >= %s%s
		ORDER  BY bs.backup_finish_date, bs.backup_set_id`,
		toUTC("bs.backup_start_date"), toUTC("bs.backup_finish_date"), sinceExpr, filter)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyHistoryError(err)
	}
	defer func() { _ = rows.Close() }()

	var out []*fwv1.ObservedBackup
	for rows.Next() {
		var (
			externalID, database, backupType        string
			startedAt, finishedAt                   time.Time
			sizeBytes                               int64
			isDamaged, hasChecksums                 bool
			userName, recoveryModel, physicalDevice string
		)
		if err := rows.Scan(&externalID, &database, &backupType, &startedAt, &finishedAt,
			&sizeBytes, &isDamaged, &hasChecksums, &userName, &recoveryModel,
			&physicalDevice); err != nil {
			return nil, sdk.Internal("read a row of the instance's backup history").WithCause(err)
		}

		if !clock.exact() {
			startedAt = startedAt.Add(-clock.offset)
			finishedAt = finishedAt.Add(-clock.offset)
		}

		out = append(out, &fwv1.ObservedBackup{
			ExternalId: strings.TrimSpace(externalID),
			Database:   database,
			Method:     backupTypeName(backupType),
			Kind:       fwv1.BackupKind_BACKUP_KIND_PHYSICAL,
			// A row exists, so the backup completed. is_damaged is the engine saying it wrote the
			// set anyway, over a database it had already found damage in — evidence about the
			// artifact, which is the one thing FAILED is reserved for (ADR-0022).
			Outcome:                 damagedOutcome(isDamaged),
			StartedAt:               timestamppb.New(startedAt.UTC()),
			FinishedAt:              timestamppb.New(finishedAt.UTC()),
			FinishedAtIsApproximate: !clock.exact(),
			SizeBytes:               sizeBytes,
			Location:                physicalDevice,
			Details: map[string]string{
				"backup_type":          backupType,
				"recovery_model":       recoveryModel,
				"taken_by":             userName,
				"has_backup_checksums": strconv.FormatBool(hasChecksums),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, classifyHistoryError(err)
	}
	return out, nil
}

func damagedOutcome(isDamaged bool) fwv1.ObservedOutcome {
	if isDamaged {
		return fwv1.ObservedOutcome_OBSERVED_OUTCOME_FAILED
	}
	return fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED
}

// backupTypeName renders msdb's one-character type code in the engine's own vocabulary. It is
// display only; core reads BackupKind, which is the same for all of them here.
func backupTypeName(code string) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "D":
		return "database"
	case "I":
		return "differential"
	case "L":
		return "log"
	case "F":
		return "file"
	case "G":
		return "differential file"
	case "P":
		return "partial"
	case "Q":
		return "differential partial"
	default:
		return "unknown"
	}
}

// observedIdentity reads back the identity the engine gave a backup this plugin has just taken.
//
// It exists so that Fleetward's own backup and the row msdb wrote about it converge on one record
// rather than appearing twice, once under each origin (ADR-0027). The device name is the file this
// backup wrote, which no other backup on this instance shares.
//
// Best effort by design: a backup that has been written, uploaded, and checksummed must not fail
// because one extra SELECT against msdb did not answer.
func observedIdentity(ctx context.Context, db *sql.DB, enginePath string) string {
	var id string
	err := db.QueryRowContext(ctx, `
		SELECT TOP (1) COALESCE(CONVERT(char(36), bs.backup_set_uuid),
		                        CONCAT('backup_set_id:', CONVERT(varchar(20), bs.backup_set_id)))
		FROM   msdb.dbo.backupset AS bs
		JOIN   msdb.dbo.backupmediafamily AS mf ON mf.media_set_id = bs.media_set_id
		WHERE  mf.physical_device_name = @path
		ORDER  BY bs.backup_set_id DESC`, sql.Named("path", enginePath)).Scan(&id)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

// classifyHistoryError says which of three different things went wrong, and the distinction is what
// makes the answer actionable: a permission problem is one GRANT away, a connection problem is the
// network, and anything else is this plugin's own fault.
//
// The third branch is the one worth having. Everything the engine answers with is an error from a
// connection, so falling back to "connect to the instance" for all of them would report a rejected
// query as an unreachable server and send somebody to check a firewall that is fine.
func classifyHistoryError(err error) error {
	var engineErr mssqlError
	if asEngineError(err, &engineErr) {
		if engineErr.hasNumber(229, 230, 916, 262, 297) {
			return sdk.PermissionDenied(
				"this login cannot read %s; observing backup history needs SELECT on "+
					"msdb.dbo.backupset and msdb.dbo.backupmediafamily, which db_datareader in msdb "+
					"grants", historySource).WithCause(err)
		}
		// The engine answered, and what it said was no. The message is the engine's own and carries
		// no credential.
		return sdk.Internal("read %s: %s", historySource, engineErr.message).WithCause(err)
	}
	return classifyConnError(err)
}

// nonEmpty drops blank entries from a repeated string field, so an empty element in a request
// cannot turn into a filter that matches nothing.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
