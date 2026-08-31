package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// defaultVerifyTimeout bounds a verification when the request carries no timeout. Counting every
// row of a restored database is the expensive part, and it is proportional to the database rather
// than to anything this plugin controls.
const defaultVerifyTimeout = 30 * time.Minute

// supportedChecks are the verification checks this plugin implements, in the order they run.
//
// Order matters: connectivity first, because everything after it is meaningless if the restored
// server does not answer; presence before counts, because "the table is missing" is a more useful
// report than "the table has zero rows".
func supportedChecks() []fwv1.VerificationCheck {
	return []fwv1.VerificationCheck{
		fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
		fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
	}
}

// VerifyRestore smoke-tests an instance that Restore has just populated, against the manifest
// captured when the backup was taken.
//
// This is the RPC the product exists for, and the shape of its answer is the reason it is a set of
// per-object discrepancies rather than a boolean: an operator woken at 3am needs to know which
// table is short by how many rows, not that "verification failed".
//
// The three statuses are genuinely different answers. VERIFIED means the restored copy matches.
// FAILED means it does not, and is the one alert that must never be routine. INCONCLUSIVE means the
// question could not be answered — no manifest, a sampled manifest, a manifest spanning databases
// this method cannot restore in one go — and reporting any of those as FAILED would train an
// operator to ignore data loss.
func (p *Plugin) VerifyRestore(ctx context.Context, req *fwv1.VerifyRestoreRequest) (*fwv1.VerifyRestoreResult, error) {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultVerifyTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := time.Now()

	checks, err := selectChecks(req.GetChecks())
	if err != nil {
		return nil, err
	}
	creds, err := restoreTargetCredentials(req.GetTarget())
	if err != nil {
		return nil, err
	}

	expected, database, reason := expectedObjects(req.GetExpected())
	if reason != "" {
		return inconclusive(req, started, reason), nil
	}

	conn, err := connect(runCtx, creds)
	if err != nil {
		// The sandbox not answering is an infrastructure problem, not evidence about the backup.
		return inconclusive(req, started,
			fmt.Sprintf("the restored instance could not be reached: %v", err)), nil
	}
	defer func() { _ = conn.Close(context.WithoutCancel(runCtx)) }()

	results := make([]*fwv1.CheckResult, 0, len(checks))
	var actual *fwv1.SourceManifest

	for _, check := range checks {
		checkStarted := time.Now()

		switch check {
		case fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY:
			results = append(results, checkConnectivity(runCtx, conn, checkStarted))
			continue
		case fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
			fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS:
		default:
			// selectChecks already rejected anything else; this keeps the switch total.
			continue
		}

		if actual == nil {
			// Counted with the very code that produced the manifest at the source — the same
			// exclusions for partitions, foreign tables, and materialized views. A second,
			// independently written counting query would disagree with the manifest on any
			// database using those features, and the disagreement would surface as a verification
			// discrepancy: a false alarm on a perfectly good backup.
			//
			// The one thing that legitimately differs is the transaction. The source counted inside
			// its exported snapshot because it had concurrent writers; a freshly restored sandbox
			// has none.
			counted, countErr := collectManifest(runCtx, conn, database, time.Now())
			if countErr != nil {
				return inconclusive(req, started,
					fmt.Sprintf("the restored instance could not be counted: %v", countErr)), nil
			}
			actual = counted
		}

		if check == fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE {
			results = append(results, checkSchemaPresence(expected, actual, checkStarted))
		} else {
			results = append(results, checkRecordCounts(expected, actual, checkStarted))
		}
	}

	return &fwv1.VerifyRestoreResult{
		VerificationId: req.GetVerificationId(),
		Status:         statusFrom(results),
		Checks:         results,
		Duration:       durationpb.New(time.Since(started)),
		Report:         report(results, expected, actual),
	}, nil
}

// selectChecks resolves the requested checks against what this plugin implements. An empty request
// runs everything.
func selectChecks(requested []fwv1.VerificationCheck) ([]fwv1.VerificationCheck, error) {
	if len(requested) == 0 {
		return supportedChecks(), nil
	}

	wanted := make(map[fwv1.VerificationCheck]bool, len(requested))
	for _, check := range requested {
		if !containsCheck(supportedChecks(), check) {
			return nil, sdk.Unsupported("this plugin does not implement the %s check", check)
		}
		wanted[check] = true
	}

	// Returned in the plugin's own order rather than the request's, so the cheap checks still run
	// before the expensive ones no matter how the request was assembled.
	selected := make([]fwv1.VerificationCheck, 0, len(wanted))
	for _, check := range supportedChecks() {
		if wanted[check] {
			selected = append(selected, check)
		}
	}
	return selected, nil
}

func containsCheck(checks []fwv1.VerificationCheck, want fwv1.VerificationCheck) bool {
	for _, c := range checks {
		if c == want {
			return true
		}
	}
	return false
}

// expectedObjects indexes the manifest by object name and names the database it describes.
//
// A missing, empty, or sampled manifest is refused with a reason rather than compared: comparing
// zero objects to zero objects succeeds trivially and would report VERIFIED on a backup that proves
// nothing at all, which is the single most dangerous answer this RPC can give.
func expectedObjects(manifest *fwv1.SourceManifest) (map[string]*fwv1.ManifestEntry, string, string) {
	if manifest == nil || len(manifest.GetEntries()) == 0 {
		return nil, "", "the backup carries no manifest, so there is nothing to compare the " +
			"restored copy against"
	}
	if manifest.GetIsSampled() {
		return nil, "", "the manifest was sampled, and this plugin only compares exact counts"
	}

	entries := make(map[string]*fwv1.ManifestEntry, len(manifest.GetEntries()))
	databases := make(map[string]bool, 1)

	for _, entry := range manifest.GetEntries() {
		if entry.GetObjectName() == "" {
			return nil, "", "the manifest contains an entry with no object name"
		}
		if _, duplicate := entries[entry.GetObjectName()]; duplicate {
			return nil, "", fmt.Sprintf("the manifest names %s twice", entry.GetObjectName())
		}
		entries[entry.GetObjectName()] = entry
		databases[entry.GetDatabase()] = true
	}

	if len(databases) > 1 {
		// The pg_dump method restores one database into one sandbox. Comparing a multi-database
		// manifest against it would report every absent database as data loss.
		return nil, "", fmt.Sprintf(
			"the manifest spans %d databases; the %s method restores one at a time", len(databases), MethodPgDump)
	}

	database := ""
	for name := range databases {
		database = name
	}
	return entries, database, ""
}

// checkConnectivity confirms the restored server answers and reports a version.
func checkConnectivity(ctx context.Context, conn queryer, started time.Time) *fwv1.CheckResult {
	version, err := readServerVersion(ctx, conn)
	if err != nil {
		return &fwv1.CheckResult{
			Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			Passed:   false,
			Severity: fwv1.Severity_SEVERITY_CRITICAL,
			Message:  fmt.Sprintf("the restored instance did not answer: %v", err),
			Duration: durationpb.New(time.Since(started)),
		}
	}

	return &fwv1.CheckResult{
		Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
		Passed:   true,
		Severity: fwv1.Severity_SEVERITY_INFO,
		Message:  "the restored instance accepts connections and reports version " + version,
		Duration: durationpb.New(time.Since(started)),
	}
}

// checkSchemaPresence asserts that every object the manifest named exists in the restored copy, and
// that nothing else does.
//
// The second half matters as much as the first. An object present in the restored copy that the
// manifest never recorded means the artifact and its manifest describe different databases, and a
// count comparison over only the intersection would happily call that VERIFIED.
func checkSchemaPresence(expected map[string]*fwv1.ManifestEntry, actual *fwv1.SourceManifest, started time.Time) *fwv1.CheckResult {
	present := make(map[string]bool, len(actual.GetEntries()))
	for _, entry := range actual.GetEntries() {
		present[entry.GetObjectName()] = true
	}

	var discrepancies []*fwv1.Discrepancy
	for _, name := range sortedNames(expected) {
		if present[name] {
			continue
		}
		entry := expected[name]
		discrepancies = append(discrepancies, &fwv1.Discrepancy{
			Database:   entry.GetDatabase(),
			ObjectName: name,
			Expected:   1,
			Actual:     0,
			Detail:     "the object is in the manifest but not in the restored copy",
		})
	}

	for _, entry := range actual.GetEntries() {
		if _, wanted := expected[entry.GetObjectName()]; wanted {
			continue
		}
		discrepancies = append(discrepancies, &fwv1.Discrepancy{
			Database:   entry.GetDatabase(),
			ObjectName: entry.GetObjectName(),
			Expected:   0,
			Actual:     1,
			Detail:     "the object is in the restored copy but not in the manifest",
		})
	}

	result := &fwv1.CheckResult{
		Check:         fwv1.VerificationCheck_VERIFICATION_CHECK_SCHEMA_PRESENCE,
		Passed:        len(discrepancies) == 0,
		Severity:      fwv1.Severity_SEVERITY_INFO,
		Discrepancies: discrepancies,
		Duration:      durationpb.New(time.Since(started)),
		Message:       fmt.Sprintf("all %d objects the manifest recorded are present", len(expected)),
	}
	if !result.GetPassed() {
		result.Severity = fwv1.Severity_SEVERITY_CRITICAL
		result.Message = fmt.Sprintf("%d of %d objects do not match the manifest",
			len(discrepancies), len(expected))
	}
	return result
}

// checkRecordCounts compares per-object row counts against the manifest.
func checkRecordCounts(expected map[string]*fwv1.ManifestEntry, actual *fwv1.SourceManifest, started time.Time) *fwv1.CheckResult {
	counted := make(map[string]int64, len(actual.GetEntries()))
	for _, entry := range actual.GetEntries() {
		counted[entry.GetObjectName()] = entry.GetRecordCount()
	}

	var (
		discrepancies []*fwv1.Discrepancy
		expectedTotal int64
		actualTotal   int64
	)

	for _, name := range sortedNames(expected) {
		entry := expected[name]
		expectedTotal += entry.GetRecordCount()

		got, present := counted[name]
		if !present {
			// Reported by the presence check with a clearer explanation; counting a missing table
			// as a count mismatch would say the same thing twice and less well.
			continue
		}
		actualTotal += got

		if got != entry.GetRecordCount() {
			discrepancies = append(discrepancies, &fwv1.Discrepancy{
				Database:   entry.GetDatabase(),
				ObjectName: name,
				Expected:   entry.GetRecordCount(),
				Actual:     got,
				Detail:     "the restored copy holds a different number of rows",
			})
		}
	}

	result := &fwv1.CheckResult{
		Check:         fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
		Passed:        len(discrepancies) == 0,
		Severity:      fwv1.Severity_SEVERITY_INFO,
		Discrepancies: discrepancies,
		Duration:      durationpb.New(time.Since(started)),
		Message: fmt.Sprintf("%d rows across %d objects match the manifest exactly",
			actualTotal, len(expected)),
	}
	if !result.GetPassed() {
		result.Severity = fwv1.Severity_SEVERITY_CRITICAL
		result.Message = fmt.Sprintf("%d objects hold the wrong number of rows: %d restored, %d expected",
			len(discrepancies), actualTotal, expectedTotal)
	}
	return result
}

// statusFrom folds the per-check results into the single status core stores and alerts on.
func statusFrom(results []*fwv1.CheckResult) fwv1.VerificationStatus {
	if len(results) == 0 {
		return fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE
	}
	for _, r := range results {
		if !r.GetPassed() {
			return fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED
		}
	}
	return fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED
}

// inconclusive builds the answer for a question that could not be asked.
func inconclusive(req *fwv1.VerifyRestoreRequest, started time.Time, reason string) *fwv1.VerifyRestoreResult {
	return &fwv1.VerifyRestoreResult{
		VerificationId: req.GetVerificationId(),
		Status:         fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE,
		Duration:       durationpb.New(time.Since(started)),
		Report:         "verification could not reach a conclusion: " + reason,
	}
}

// report renders the human-readable summary the UI shows and the alert quotes.
func report(results []*fwv1.CheckResult, expected map[string]*fwv1.ManifestEntry, actual *fwv1.SourceManifest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "compared %d objects against the backup manifest", len(expected))
	if actual != nil {
		fmt.Fprintf(&b, "; the restored copy holds %d objects and %d rows",
			actual.GetTotalObjects(), actual.GetTotalRecords())
	}

	for _, r := range results {
		outcome := "passed"
		if !r.GetPassed() {
			outcome = "FAILED"
		}
		fmt.Fprintf(&b, "\n%s: %s — %s", checkName(r.GetCheck()), outcome, r.GetMessage())

		// Only the first few discrepancies are quoted: a database that restored empty would
		// otherwise produce a report one line per table long, and the structured discrepancies are
		// carried in full on the check itself.
		for i, d := range r.GetDiscrepancies() {
			if i == 5 {
				fmt.Fprintf(&b, "\n  … and %d more", len(r.GetDiscrepancies())-i)
				break
			}
			fmt.Fprintf(&b, "\n  %s: expected %d, found %d", d.GetObjectName(), d.GetExpected(), d.GetActual())
		}
	}

	return b.String()
}

// checkName renders a check for a human, without the enum prefix the operator never asked about.
func checkName(check fwv1.VerificationCheck) string {
	return strings.ToLower(strings.TrimPrefix(check.String(), "VERIFICATION_CHECK_"))
}

// sortedNames gives the checks a stable order, so two runs of the same verification produce the
// same report and a diff between them means something changed.
func sortedNames(entries map[string]*fwv1.ManifestEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
