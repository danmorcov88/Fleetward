package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// integrityTimeout bounds DBCC CHECKDB. It is the slowest check by a wide margin, and a sandbox
// that has stopped answering must not hold a verification open indefinitely.
const integrityTimeout = 30 * time.Minute

// VerifyRestore smoke-tests an instance Restore has just populated, against the manifest captured
// when the backup was taken.
//
// This is the RPC the product exists for, and its hardest requirement is not running the checks —
// it is reporting the right kind of wrong. FAILED means the artifact does not hold what it claimed
// to hold. Everything else — a sandbox that stopped answering, a manifest that is not there — is
// INCONCLUSIVE, because an alert that fires on infrastructure trouble is an alert nobody reads
// (ADR-0022).
func (p *Plugin) VerifyRestore(ctx context.Context, req *fwv1.VerifyRestoreRequest) (*fwv1.VerifyRestoreResult, error) {
	started := time.Now()

	expected := req.GetExpected()
	if len(expected.GetEntries()) == 0 {
		// Comparing zero objects to zero objects succeeds trivially, so the naive implementation
		// reports VERIFIED for a backup that proves nothing at all. Core refuses before it ever
		// provisions a sandbox; this is the other half of the same check, because the two are
		// separately implementable and this is not a check to have in only one of them.
		return inconclusive(req, started,
			"the backup carries no manifest, so there is nothing to compare the restored copy against"), nil
	}

	creds, err := restoreTargetCredentials(req.GetTarget())
	if err != nil {
		return nil, err
	}

	database := strings.TrimSpace(expected.GetEntries()[0].GetDatabase())
	if database == "" {
		database = strings.TrimSpace(creds.GetDatabase())
	}
	if database == "" {
		return inconclusive(req, started, "the manifest does not say which database it describes"), nil
	}

	db, err := open(creds, database)
	if err != nil {
		return inconclusive(req, started, "the restored instance could not be connected to"), nil
	}
	defer func() { _ = db.Close() }()

	checks := make([]*fwv1.CheckResult, 0, 5)

	connectivity, version := checkConnectivity(ctx, db, database)
	checks = append(checks, connectivity)
	if !connectivity.GetPassed() {
		// Every remaining check needs a working connection, so running them would produce four more
		// failures that all say the same thing.
		return assemble(req, started, checks, version,
			fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE), nil
	}

	actual, err := tableCounts(ctx, db)
	if err != nil {
		checks = append(checks, failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
			"the restored database's tables could not be listed"))
		return assemble(req, started, checks, version,
			fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE), nil
	}

	presence := checkSchemaPresence(expected, actual)
	counts := checkRecordCounts(expected, actual)
	checks = append(checks, presence, counts)
	checks = append(checks, checkQueryability(ctx, db, expected))
	checks = append(checks, checkIntegrity(ctx, db, database))

	return assemble(req, started, checks, version, verdict(checks, counts, presence)), nil
}

// verdict turns the individual checks into the one answer an operator acts on.
//
// The asymmetry is the whole point. A missing object or a count that is short is evidence about the
// artifact and fails the verification. A check that could not run — DBCC that timed out, a query
// that lost its connection — is not, and neither is a count on an object the manifest itself admits
// it could not pin to the artifact's consistency point.
func verdict(checks []*fwv1.CheckResult, counts, presence *fwv1.CheckResult) fwv1.VerificationStatus {
	if !presence.GetPassed() || !counts.GetPassed() {
		return fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED
	}
	for _, check := range checks {
		if !check.GetPassed() {
			return fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE
		}
	}
	return fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED
}

// checkConnectivity confirms the restored database is online and answering.
func checkConnectivity(ctx context.Context, db *sql.DB, database string) (*fwv1.CheckResult, string) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var (
		version string
		state   string
	)
	err := db.QueryRowContext(ctx, `
		SELECT CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128)),
		       CAST(DATABASEPROPERTYEX(DB_NAME(), 'Status') AS nvarchar(64))`).Scan(&version, &state)
	if err != nil {
		return failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			"the restored instance did not answer"), ""
	}
	if !strings.EqualFold(state, "ONLINE") {
		return failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			fmt.Sprintf("%s is %s rather than ONLINE", database, state)), version
	}

	return &fwv1.CheckResult{
		Check:   fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		Passed:  true,
		Message: fmt.Sprintf("%s is online on SQL Server %s", database, version),
	}, version
}

// checkSchemaPresence asserts every object the manifest names exists in the restored copy.
func checkSchemaPresence(expected *fwv1.SourceManifest, actual map[string]int64) *fwv1.CheckResult {
	var missing []*fwv1.Discrepancy
	for _, entry := range expected.GetEntries() {
		if _, ok := actual[entry.GetObjectName()]; !ok {
			missing = append(missing, &fwv1.Discrepancy{
				Database:   entry.GetDatabase(),
				ObjectName: entry.GetObjectName(),
				Expected:   entry.GetRecordCount(),
				Actual:     0,
				Detail:     "the object is not present in the restored copy",
			})
		}
	}

	if len(missing) > 0 {
		return &fwv1.CheckResult{
			Check:         fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
			Passed:        false,
			Message:       fmt.Sprintf("%d of %d objects are missing", len(missing), len(expected.GetEntries())),
			Discrepancies: missing,
		}
	}
	return &fwv1.CheckResult{
		Check:   fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
		Passed:  true,
		Message: fmt.Sprintf("all %d objects are present", len(expected.GetEntries())),
	}
}

// checkRecordCounts compares the restored copy against the manifest, object by object.
//
// A mismatch names the object and both numbers. An operator woken at 3am needs to know which table
// is short by how many rows, not that "verification failed".
//
// An entry the manifest flagged as possibly drifted is reported and does not fail the check. That
// flag means the source was written to while the backup ran, so the number was never a statement
// about the artifact in the first place — treating it as data loss would be the false alarm this
// whole design is built to avoid.
func checkRecordCounts(expected *fwv1.SourceManifest, actual map[string]int64) *fwv1.CheckResult {
	var (
		mismatches []*fwv1.Discrepancy
		drifted    []*fwv1.Discrepancy
		compared   int
	)

	for _, entry := range expected.GetEntries() {
		got, ok := actual[entry.GetObjectName()]
		if !ok {
			// Reported by the schema-presence check; counting a missing object twice would say the
			// same thing in two places.
			continue
		}
		compared++
		if got == entry.GetRecordCount() {
			continue
		}

		discrepancy := &fwv1.Discrepancy{
			Database:   entry.GetDatabase(),
			ObjectName: entry.GetObjectName(),
			Expected:   entry.GetRecordCount(),
			Actual:     got,
			Detail: fmt.Sprintf("the restored copy holds %d rows where the manifest recorded %d",
				got, entry.GetRecordCount()),
		}
		if entry.GetCountMayHaveDrifted() {
			discrepancy.Detail += "; the source was being written to while the backup ran, so this " +
				"is drift rather than data loss"
			drifted = append(drifted, discrepancy)
			continue
		}
		mismatches = append(mismatches, discrepancy)
	}

	switch {
	case len(mismatches) > 0:
		return &fwv1.CheckResult{
			Check:  fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
			Passed: false,
			Message: fmt.Sprintf("%d of %d objects do not match the manifest",
				len(mismatches), compared),
			Discrepancies: append(mismatches, drifted...),
		}
	case len(drifted) > 0:
		// Passed, but not silently: the report says what was waved through and why, so nobody has
		// to trust that it was reasonable.
		return &fwv1.CheckResult{
			Check:  fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
			Passed: true,
			Message: fmt.Sprintf("%d objects match; %d differ on counts the manifest could not pin "+
				"to the artifact", compared-len(drifted), len(drifted)),
			Discrepancies: drifted,
		}
	default:
		return &fwv1.CheckResult{
			Check:   fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
			Passed:  true,
			Message: fmt.Sprintf("all %d objects match the manifest exactly", compared),
		}
	}
}

// checkQueryability reads from the restored data rather than only counting it.
//
// A count can be answered from an index. This one asks for rows, which is the difference between
// "the metadata survived" and "the data can be read back".
func checkQueryability(ctx context.Context, db *sql.DB, expected *fwv1.SourceManifest) *fwv1.CheckResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	const sample = 3
	var read int
	for _, entry := range expected.GetEntries() {
		if read >= sample {
			break
		}
		if entry.GetRecordCount() == 0 {
			continue
		}
		schema, table := splitObjectName(entry.GetObjectName())
		// The identifier comes from a manifest this plugin wrote, and is bracket-quoted the way
		// QUOTENAME quotes it.
		query := "SELECT TOP 1 1 FROM " + qualified(schema, table)

		var one int
		if err := db.QueryRowContext(ctx, query).Scan(&one); err != nil {
			return failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_QUERYABILITY,
				fmt.Sprintf("%s could not be read back", entry.GetObjectName()))
		}
		read++
	}

	return &fwv1.CheckResult{
		Check:   fwv1.VerificationCheck_VERIFICATION_CHECK_QUERYABILITY,
		Passed:  true,
		Message: fmt.Sprintf("%d objects were read back successfully", read),
	}
}

// checkIntegrity runs the engine's own consistency check over the restored copy.
//
// This is the check a DBA would run by hand, and the reason restoring a backup somewhere is worth
// doing at all: it reads every page and validates it. PHYSICAL_ONLY keeps it to the physical
// structure, which is what a restore can damage, and is what makes it affordable on a real database.
func checkIntegrity(ctx context.Context, db *sql.DB, database string) *fwv1.CheckResult {
	ctx, cancel := context.WithTimeout(ctx, integrityTimeout)
	defer cancel()

	//nolint:gosec // G202: DBCC takes a database name rather than a parameter, escaped as QUOTENAME
	// escapes it, and this one is the sandbox's own.
	stmt := "DBCC CHECKDB (" + quoteIdentifier(database) + ") WITH PHYSICAL_ONLY, NO_INFOMSGS"
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		// DBCC reports corruption as an error, and so does a connection that died mid-check. The
		// two are told apart the same way everything else on this path is: by the number.
		var engineErr mssqlError
		if asEngineError(err, &engineErr) {
			return failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY,
				"DBCC CHECKDB reported a problem: "+engineMessage(err))
		}
		return failedCheck(fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY,
			"DBCC CHECKDB could not be completed")
	}

	return &fwv1.CheckResult{
		Check:   fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY,
		Passed:  true,
		Message: "DBCC CHECKDB found no physical inconsistencies",
	}
}

// tableCounts reads every user table's row count from the restored copy.
func tableCounts(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	tables, err := listTables(ctx, db)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		query := "SELECT COUNT_BIG(*) FROM " + qualified(table.schema, table.name)
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, sdk.Internal("count rows in %s", objectName(table.schema, table.name)).
				WithCause(classifyConnError(err))
		}
		counts[objectName(table.schema, table.name)] = count
	}
	return counts, nil
}

// failedCheck is one check that did not pass, with the reason.
func failedCheck(check fwv1.VerificationCheck, message string) *fwv1.CheckResult {
	return &fwv1.CheckResult{Check: check, Passed: false, Message: message}
}

// inconclusive is the answer when verification could not run at all.
func inconclusive(req *fwv1.VerifyRestoreRequest, started time.Time, reason string) *fwv1.VerifyRestoreResult {
	return &fwv1.VerifyRestoreResult{
		VerificationId: req.GetVerificationId(),
		Status:         fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		Report:         reason,
		Duration:       durationpb.New(time.Since(started)),
	}
}

// assemble builds the result and the human-readable report.
func assemble(
	req *fwv1.VerifyRestoreRequest,
	started time.Time,
	checks []*fwv1.CheckResult,
	version string,
	status fwv1.VerificationStatus,
) *fwv1.VerifyRestoreResult {
	// Sorted by check so two runs of the same verification read the same way, which matters when
	// someone is comparing last night's report with tonight's.
	sort.SliceStable(checks, func(i, j int) bool { return checks[i].GetCheck() < checks[j].GetCheck() })

	var report strings.Builder
	fmt.Fprintf(&report, "SQL Server %s, verified in %s\n", version, time.Since(started).Truncate(time.Millisecond))
	for _, check := range checks {
		mark := "ok  "
		if !check.GetPassed() {
			mark = "FAIL"
		}
		fmt.Fprintf(&report, "%s %s: %s\n", mark, checkName(check.GetCheck()), check.GetMessage())
		for _, d := range check.GetDiscrepancies() {
			fmt.Fprintf(&report, "       %s: expected %d, found %d — %s\n",
				d.GetObjectName(), d.GetExpected(), d.GetActual(), d.GetDetail())
		}
	}

	return &fwv1.VerifyRestoreResult{
		VerificationId: req.GetVerificationId(),
		Status:         status,
		Checks:         checks,
		Report:         strings.TrimRight(report.String(), "\n"),
		Duration:       durationpb.New(time.Since(started)),
	}
}

// checkName renders a check for a human, without the enum's prefix.
func checkName(check fwv1.VerificationCheck) string {
	return strings.ToLower(strings.TrimPrefix(check.String(), "VERIFICATION_CHECK_"))
}
