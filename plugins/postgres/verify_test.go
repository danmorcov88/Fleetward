package postgres

import (
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// manifest builds a source manifest from object/count pairs.
func manifest(database string, counts map[string]int64) *fwv1.SourceManifest {
	m := &fwv1.SourceManifest{TotalObjects: int64(len(counts))}
	for name, count := range counts {
		m.Entries = append(m.Entries, &fwv1.ManifestEntry{
			Database: database, ObjectName: name, RecordCount: count,
		})
		m.TotalRecords += count
	}
	return m
}

// TestAnEmptyManifestIsNeverVerified is the trap this slice exists to avoid. Comparing zero objects
// to zero objects succeeds trivially, and reporting VERIFIED for it would be the most dangerous
// answer this plugin can give: a backup that proves nothing, presented as proven.
func TestAnEmptyManifestIsNeverVerified(t *testing.T) {
	tests := []struct {
		name string
		in   *fwv1.SourceManifest
	}{
		{"nil", nil},
		{"empty", &fwv1.SourceManifest{}},
		{"no entries but a total", &fwv1.SourceManifest{TotalObjects: 7, TotalRecords: 900}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, _, reason := expectedObjects(tc.in)
			if reason == "" {
				t.Fatal("an empty manifest was accepted; verification would pass trivially")
			}
			if entries != nil {
				t.Error("entries were returned alongside a refusal")
			}
		})
	}
}

// TestASampledManifestIsInconclusive guards the semantic difference recorded in slice A4: sampling
// is not a faster version of counting, it is a comparison within a tolerance this plugin does not
// implement. Comparing sampled estimates for equality would fail every healthy backup.
func TestASampledManifestIsInconclusive(t *testing.T) {
	m := manifest("app", map[string]int64{"public.customers": 40})
	m.IsSampled = true

	if _, _, reason := expectedObjects(m); reason == "" {
		t.Fatal("a sampled manifest was compared for exact equality")
	}
}

func TestExpectedObjectsRefusesAMultiDatabaseManifest(t *testing.T) {
	m := &fwv1.SourceManifest{Entries: []*fwv1.ManifestEntry{
		{Database: "app", ObjectName: "public.customers", RecordCount: 1},
		{Database: "analytics", ObjectName: "public.events", RecordCount: 1},
	}}

	// One sandbox holds one restored database. Comparing a two-database manifest against it would
	// report the absent one as data loss.
	if _, _, reason := expectedObjects(m); reason == "" {
		t.Fatal("a manifest spanning two databases was accepted")
	}
}

func TestExpectedObjectsIndexesASingleDatabase(t *testing.T) {
	entries, database, reason := expectedObjects(manifest("app", map[string]int64{
		"public.customers": 40,
		"public.orders":    120,
	}))
	if reason != "" {
		t.Fatalf("a well-formed manifest was refused: %s", reason)
	}
	if database != "app" {
		t.Errorf("database = %q, want app", database)
	}
	if len(entries) != 2 || entries["public.orders"].GetRecordCount() != 120 {
		t.Errorf("entries = %v, want the two objects with their counts", entries)
	}
}

func TestCheckRecordCountsNamesEveryDiscrepancy(t *testing.T) {
	expected, _, _ := expectedObjects(manifest("app", map[string]int64{
		"public.customers": 40,
		"public.orders":    120,
		"public.invoices":  7,
	}))
	actual := manifest("app", map[string]int64{
		"public.customers": 40,
		"public.orders":    118,
		"public.invoices":  0,
	})

	result := checkRecordCounts(expected, actual, time.Now())

	if result.GetPassed() {
		t.Fatal("a short restore passed the record count check")
	}
	if result.GetSeverity() != fwv1.Severity_SEVERITY_CRITICAL {
		t.Errorf("severity = %v, want CRITICAL", result.GetSeverity())
	}
	if len(result.GetDiscrepancies()) != 2 {
		t.Fatalf("reported %d discrepancies, want 2", len(result.GetDiscrepancies()))
	}

	// Sorted by object name, so two runs of the same verification produce the same report.
	first := result.GetDiscrepancies()[0]
	if first.GetObjectName() != "public.invoices" || first.GetExpected() != 7 || first.GetActual() != 0 {
		t.Errorf("first discrepancy = %v, want public.invoices 7 → 0", first)
	}
	// Expected and actual both travel, because "orders is short" is not actionable and "orders is
	// short by two rows" is.
	second := result.GetDiscrepancies()[1]
	if second.GetExpected() != 120 || second.GetActual() != 118 {
		t.Errorf("second discrepancy = %d → %d, want 120 → 118", second.GetExpected(), second.GetActual())
	}
}

func TestCheckRecordCountsPassesOnAnExactMatch(t *testing.T) {
	counts := map[string]int64{"public.customers": 40, "public.orders": 120}
	expected, _, _ := expectedObjects(manifest("app", counts))

	result := checkRecordCounts(expected, manifest("app", counts), time.Now())
	if !result.GetPassed() {
		t.Fatalf("an exact match failed: %s", result.GetMessage())
	}
	if len(result.GetDiscrepancies()) != 0 {
		t.Errorf("an exact match reported %d discrepancies", len(result.GetDiscrepancies()))
	}
}

// TestCheckSchemaPresenceReportsBothDirections covers the half that is easy to forget. A table
// present in the restored copy that the manifest never recorded means the artifact and its manifest
// describe different databases, and comparing only the intersection would call that VERIFIED.
func TestCheckSchemaPresenceReportsBothDirections(t *testing.T) {
	expected, _, _ := expectedObjects(manifest("app", map[string]int64{
		"public.customers": 40,
		"public.orders":    120,
	}))
	actual := manifest("app", map[string]int64{
		"public.customers": 40,
		"public.surprise":  3,
	})

	result := checkSchemaPresence(expected, actual, time.Now())
	if result.GetPassed() {
		t.Fatal("a restore missing one table and holding an unexpected one passed")
	}

	var missing, unexpected bool
	for _, d := range result.GetDiscrepancies() {
		switch d.GetObjectName() {
		case "public.orders":
			missing = true
		case "public.surprise":
			unexpected = true
		}
	}
	if !missing {
		t.Error("the missing object was not reported")
	}
	if !unexpected {
		t.Error("the unexpected object was not reported")
	}
}

func TestSelectChecks(t *testing.T) {
	tests := []struct {
		name      string
		requested []fwv1.VerificationCheck
		want      int
		wantErr   bool
	}{
		{"empty runs everything", nil, len(supportedChecks()), false},
		{
			name:      "a subset is honoured",
			requested: []fwv1.VerificationCheck{fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY},
			want:      1,
		},
		{
			// Refused rather than silently skipped: a caller that asked for an integrity check and
			// got a green result without one has been told something untrue.
			name:      "an unimplemented check is refused",
			requested: []fwv1.VerificationCheck{fwv1.VerificationCheck_VERIFICATION_CHECK_INTEGRITY},
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectChecks(tc.requested)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected a refusal")
				}
				return
			}
			if err != nil {
				t.Fatalf("selectChecks: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("selected %d checks, want %d", len(got), tc.want)
			}
		})
	}
}

// TestSelectChecksRunsCheapChecksFirst matters because a database that never started should be
// reported as unreachable rather than as every table being empty.
func TestSelectChecksRunsCheapChecksFirst(t *testing.T) {
	got, err := selectChecks([]fwv1.VerificationCheck{
		fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
		fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
	})
	if err != nil {
		t.Fatalf("selectChecks: %v", err)
	}
	if got[0] != fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY {
		t.Errorf("first check = %v, want CONNECTIVITY regardless of request order", got[0])
	}
}

func TestStatusFrom(t *testing.T) {
	pass := &fwv1.CheckResult{Passed: true}
	fail := &fwv1.CheckResult{Passed: false}

	tests := []struct {
		name string
		in   []*fwv1.CheckResult
		want fwv1.VerificationStatus
	}{
		{"all passed", []*fwv1.CheckResult{pass, pass}, fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED},
		{"one failed", []*fwv1.CheckResult{pass, fail}, fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED},
		// No checks means nothing was proven, and VERIFIED would be the wrong default.
		{"none ran", nil, fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusFrom(tc.in); got != tc.want {
				t.Errorf("statusFrom = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSandboxTemplateDerivesItsTagFromTheArtifactsVersion is the trap named in the brief:
// restoring a PostgreSQL 16 dump into a 15 container fails in ways that look like data corruption.
// The template is what lets core resolve the right image, and core resolves it against the version
// recorded on the backup rather than the version the instance runs today.
//
// Tag rendering itself is core's, and is tested in internal/controlplane/sandbox. What is asserted
// here is that this plugin asks for a major-version tag at all, and that its fallback is a real
// version rather than "latest" — verifying against an unknown engine version is worse than
// reporting that a backup could not be verified.
func TestSandboxTemplateDerivesItsTagFromTheArtifactsVersion(t *testing.T) {
	tmpl := sandboxTemplate()

	if tmpl.GetImageRepository() != "postgres" {
		t.Errorf("image_repository = %q, want postgres", tmpl.GetImageRepository())
	}
	if tmpl.GetTagTemplate() != "{{ .Major }}" {
		t.Errorf("tag_template = %q, want the major version of whatever produced the artifact",
			tmpl.GetTagTemplate())
	}
	if tmpl.GetDefaultTag() == "" || tmpl.GetDefaultTag() == "latest" {
		t.Errorf("default_tag = %q; a sandbox must never be pulled at an unpinned tag", tmpl.GetDefaultTag())
	}
}

// TestSandboxTemplatePlacesTheGeneratedIdentity guards ADR-0020: core generates the credentials and
// the template is the only thing that says where they belong. A template that hard-coded a password
// would compile it into the plugin binary.
func TestSandboxTemplatePlacesTheGeneratedIdentity(t *testing.T) {
	tmpl := sandboxTemplate()

	for _, key := range []string{"POSTGRES_USER", "POSTGRES_PASSWORD", "POSTGRES_DB"} {
		value := tmpl.GetEnv()[key]
		if value == "" {
			t.Errorf("%s is not set; the sandbox would use the image's defaults", key)
			continue
		}
		if !containsTemplateAction(value) {
			t.Errorf("%s = %q, which is a literal rather than a placeholder core can fill", key, value)
		}
	}

	if tmpl.GetContainerPort() != 5432 {
		t.Errorf("container_port = %d, want 5432", tmpl.GetContainerPort())
	}

	// The readiness probe has to be scoped to TCP: initdb runs a temporary server on the unix
	// socket, and a probe that reaches it reports ready while the real server is still restarting.
	var sawLoopback bool
	for _, arg := range tmpl.GetReadinessCommand() {
		if arg == "127.0.0.1" {
			sawLoopback = true
		}
	}
	if !sawLoopback {
		t.Error("the readiness command is not scoped to TCP; it can reach initdb's temporary server")
	}
	if tmpl.GetReadinessTimeout().AsDuration() <= 0 {
		t.Error("no readiness timeout is declared")
	}
}

func containsTemplateAction(value string) bool {
	for i := 0; i+1 < len(value); i++ {
		if value[i] == '{' && value[i+1] == '{' {
			return true
		}
	}
	return false
}
