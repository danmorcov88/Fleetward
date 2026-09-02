package sqlserver

import (
	"strings"
	"testing"

	mssql "github.com/microsoft/go-mssqldb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

func TestCapabilitiesAreCoherent(t *testing.T) {
	caps, err := New().Capabilities(t.Context())
	if err != nil {
		t.Fatalf("Capabilities() error = %v", err)
	}
	if err := sdk.ValidateCapabilities(caps); err != nil {
		t.Fatalf("ValidateCapabilities() error = %v", err)
	}
	if caps.GetEngineType() != EngineType {
		t.Errorf("engine_type = %q, want %q", caps.GetEngineType(), EngineType)
	}

	method := sdk.DefaultBackupMethod(caps)
	if method == nil || method.GetId() != MethodFull {
		t.Fatalf("default backup method = %v, want %s", method, MethodFull)
	}
	if !method.GetRequiresSharedDirectory() {
		t.Error("BACKUP DATABASE writes a file on the server, so the method must declare that it " +
			"needs a shared directory — core refuses to schedule without it")
	}
	if method.GetEnablesPitr() {
		t.Error("a full backup is not a PITR baseline until log backups exist, and this slice takes none")
	}
	if len(caps.GetRequiredTools()) != 0 {
		t.Errorf("required_tools = %v; BACKUP and RESTORE are statements, not tools", caps.GetRequiredTools())
	}
	if caps.GetSupportsPitr() {
		t.Error("supports_pitr is declared without point-in-time recovery behind it")
	}
}

// TestSandboxTemplateDeclaresWhatTheImageDemands pins the three things this engine needed that
// PostgreSQL did not, each of which was measured against the real image rather than assumed.
func TestSandboxTemplateDeclaresWhatTheImageDemands(t *testing.T) {
	tmpl := sandboxTemplate()

	if tmpl.GetFixedUsername() != "sa" {
		t.Errorf("fixed_username = %q; the image creates sa and cannot be told to rename it, and the "+
			"account it can create through MSSQL_USER cannot RESTORE", tmpl.GetFixedUsername())
	}

	policy := tmpl.GetPasswordPolicy()
	if policy.GetMinCharacterClasses() < 3 {
		t.Errorf("min_character_classes = %d; the image exits 255 on a password carrying fewer than "+
			"three of the four classes", policy.GetMinCharacterClasses())
	}
	if tmpl.GetSharedDirectory() == "" {
		t.Error("no shared directory is declared, so RESTORE would have nowhere to read the artifact from")
	}

	if tmpl.GetEnv()["ACCEPT_EULA"] != "Y" {
		t.Error("ACCEPT_EULA is not set; the container exits before it starts")
	}
	// The database is created by a setup pass that runs after the server is listening, so a probe
	// against master reports ready while the database still does not exist.
	if !containsArg(tmpl.GetReadinessCommand(), "{{ .Database }}") {
		t.Error("the readiness command does not connect to the sandbox database, so it would report " +
			"ready before the image had created it")
	}

	// A password is never compiled into a plugin binary (ADR-0020).
	for key, value := range tmpl.GetEnv() {
		if strings.Contains(strings.ToLower(key), "password") && !strings.Contains(value, "{{") {
			t.Errorf("env %q carries a literal value rather than a placeholder", key)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestQuotingEscapesTheDelimiter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"a plain identifier", quoteIdentifier("orders"), "[orders]"},
		{"a closing bracket is doubled", quoteIdentifier("ord]ers"), "[ord]]ers]"},
		{"a plain literal", quoteLiteral("/backup/a.bak"), "N'/backup/a.bak'"},
		{"a quote is doubled", quoteLiteral("o'brien"), "N'o''brien'"},
		{"a qualified name", qualified("dbo", "orders"), "[dbo].[orders]"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestObjectNamesRoundTrip(t *testing.T) {
	t.Parallel()

	schema, table := splitObjectName(objectName("dbo", "orders"))
	if schema != "dbo" || table != "orders" {
		t.Errorf("round trip gave %q.%q", schema, table)
	}
	// A name with no schema is what a manifest written before schemas were qualified would carry.
	if schema, table := splitObjectName("orders"); schema != "dbo" || table != "orders" {
		t.Errorf("unqualified name gave %q.%q", schema, table)
	}
}

func TestResolveDatabase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		creds     *fwv1.Credentials
		requested []string
		want      string
		wantErr   bool
	}{
		{
			name:  "the connection's database",
			creds: &fwv1.Credentials{Database: "sales"},
			want:  "sales",
		},
		{
			name:      "an explicit request wins",
			creds:     &fwv1.Credentials{Database: "sales"},
			requested: []string{"warehouse"},
			want:      "warehouse",
		},
		{
			// Backing up master because nobody named a database would be a backup of the wrong
			// thing that looks like a backup of the right thing.
			name:    "master is refused",
			creds:   &fwv1.Credentials{Database: "master"},
			wantErr: true,
		},
		{
			name:    "no database at all",
			creds:   &fwv1.Credentials{},
			wantErr: true,
		},
		{
			name:      "more than one database",
			creds:     &fwv1.Credentials{Database: "sales"},
			requested: []string{"a", "b"},
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveDatabase(tc.creds, tc.requested)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveDatabase() = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveDatabase() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("resolveDatabase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRestoreFailureBlamesTheRightThing is the most important test in this file.
//
// Core reads a failed restore that blames the artifact as this product's one critical alert, and
// everything else as an infrastructure problem to look at later. Getting the classification wrong
// in either direction is worse than not running the check: one direction cries wolf, the other
// hides data loss.
//
// The numbers are the ones a real SQL Server produced against a real truncated and a real
// byte-flipped artifact.
func TestRestoreFailureBlamesTheRightThing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantCorrupt bool
	}{
		{
			name:        "a truncated artifact",
			err:         engineError(3203, "Read on \"/var/opt/mssql/fleetward/a.bak\" failed: 13(The data is invalid.)", 3013),
			wantCorrupt: true,
		},
		{
			name:        "a byte flipped mid-stream",
			err:         engineError(11801, "RESTORE detected one or more corrupted pages in the backup set.", 3013),
			wantCorrupt: true,
		},
		{
			name:        "not a backup set at all",
			err:         engineError(3241, "The media family on device \"…\" is incorrectly formed.", 3013),
			wantCorrupt: true,
		},
		{
			// The sandbox lost a race and stopped answering. Reporting this as data loss is what
			// makes an operator mute the alert that matters.
			name:        "the target stopped answering",
			err:         errNoNumberedMessage{},
			wantCorrupt: false,
		},
		{
			name:        "the engine cannot open the path",
			err:         engineError(3201, "Cannot open backup device. Operating system error 2.", 3013),
			wantCorrupt: false,
		},
		{
			name:        "the target refuses for its own reasons",
			err:         engineError(3101, "Exclusive access could not be obtained because the database is in use.", 3013),
			wantCorrupt: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sdk.AsPluginError(restoreFailure(tc.err))
			if sdk.IsArtifactCorrupt(got) != tc.wantCorrupt {
				t.Errorf("IsArtifactCorrupt = %v, want %v (code %s: %s)",
					sdk.IsArtifactCorrupt(got), tc.wantCorrupt, got.GetCode(), got.GetMessage())
			}
		})
	}
}

// engineError builds a driver error the way the driver builds one: a batch of numbered messages
// whose last entry is the generic "terminating abnormally", with the diagnosis earlier.
func engineError(number int32, message string, trailing ...int32) error {
	all := []mssql.Error{{Number: number, Message: message, Class: 16}}
	for _, n := range trailing {
		all = append(all, mssql.Error{Number: n, Message: "RESTORE DATABASE is terminating abnormally.", Class: 16})
	}
	last := all[len(all)-1]
	last.All = all
	return last
}

// errNoNumberedMessage stands in for a connection that ended before the server said anything.
type errNoNumberedMessage struct{}

func (errNoNumberedMessage) Error() string { return "EOF" }

// TestRecordCountsSeparateDataLossFromDrift covers the manifest flag this engine needed.
//
// A physical backup is consistent at its own ending LSN, and a COUNT(*) on a live database cannot
// be tied to that LSN without writing to the monitored instance. So an object that was written to
// while the backup ran carries a count that was never a statement about the artifact, and a
// mismatch on it must not fire the alert that means "your backup is bad".
func TestRecordCountsSeparateDataLossFromDrift(t *testing.T) {
	t.Parallel()

	manifest := &fwv1.SourceManifest{Entries: []*fwv1.ManifestEntry{
		{Database: "sales", ObjectName: "dbo.customers", RecordCount: 40},
		{Database: "sales", ObjectName: "dbo.orders", RecordCount: 120},
	}}

	t.Run("an exact shortfall is data loss", func(t *testing.T) {
		got := checkRecordCounts(manifest, map[string]int64{"dbo.customers": 40, "dbo.orders": 90})
		if got.GetPassed() {
			t.Fatal("a table 30 rows short passed the count check")
		}
		if len(got.GetDiscrepancies()) != 1 {
			t.Fatalf("reported %d discrepancies, want 1", len(got.GetDiscrepancies()))
		}
		d := got.GetDiscrepancies()[0]
		if d.GetObjectName() != "dbo.orders" || d.GetExpected() != 120 || d.GetActual() != 90 {
			t.Errorf("discrepancy = %s expected %d actual %d", d.GetObjectName(), d.GetExpected(), d.GetActual())
		}
	})

	t.Run("a flagged shortfall is drift", func(t *testing.T) {
		drifting := &fwv1.SourceManifest{Entries: []*fwv1.ManifestEntry{
			{Database: "sales", ObjectName: "dbo.customers", RecordCount: 40},
			{Database: "sales", ObjectName: "dbo.orders", RecordCount: 120, CountMayHaveDrifted: true},
		}}
		got := checkRecordCounts(drifting, map[string]int64{"dbo.customers": 40, "dbo.orders": 90})
		if !got.GetPassed() {
			t.Fatal("a count the manifest admits it could not pin to the artifact failed the check")
		}
		// Passed, but not silently: the report has to say what was waved through.
		if len(got.GetDiscrepancies()) != 1 {
			t.Errorf("the drift was not reported, so nobody can see what was waved through")
		}
	})

	t.Run("everything matching passes", func(t *testing.T) {
		got := checkRecordCounts(manifest, map[string]int64{"dbo.customers": 40, "dbo.orders": 120})
		if !got.GetPassed() || len(got.GetDiscrepancies()) != 0 {
			t.Errorf("a matching restore did not pass cleanly: %s", got.GetMessage())
		}
	})
}

func TestSchemaPresenceNamesTheMissingObject(t *testing.T) {
	t.Parallel()

	manifest := &fwv1.SourceManifest{Entries: []*fwv1.ManifestEntry{
		{Database: "sales", ObjectName: "dbo.customers", RecordCount: 40},
		{Database: "sales", ObjectName: "dbo.orders", RecordCount: 120},
	}}

	got := checkSchemaPresence(manifest, map[string]int64{"dbo.customers": 40})
	if got.GetPassed() {
		t.Fatal("a restore missing a whole table passed the presence check")
	}
	if len(got.GetDiscrepancies()) != 1 || got.GetDiscrepancies()[0].GetObjectName() != "dbo.orders" {
		t.Errorf("the report does not name the missing object: %v", got.GetDiscrepancies())
	}
}

// TestRestoreRefusesARealInstance keeps the capability matrix and the behaviour from drifting apart
// while restoring over a live instance is unimplemented.
func TestRestoreRefusesARealInstance(t *testing.T) {
	t.Parallel()

	_, err := restoreTargetCredentials(&fwv1.RestoreTarget{
		Kind:        fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_INSTANCE,
		Credentials: &fwv1.Credentials{Host: "prod-1", Port: 1433},
	})
	if err == nil {
		t.Fatal("restoring over a real instance was accepted")
	}
	if got := sdk.AsPluginError(err).GetCode(); got != fwv1.ErrorCode_ERROR_CODE_UNSUPPORTED {
		t.Errorf("code = %s, want UNSUPPORTED", got)
	}
}

// TestBackupRefusesAnInstanceWithNoSharedDirectory covers the precondition this engine introduced.
// The message has to name both halves, because configuring one of them is the usual mistake.
func TestBackupRefusesAnInstanceWithNoSharedDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		share *fwv1.SharedDirectory
	}{
		{"none at all", nil},
		{"only the engine's half", &fwv1.SharedDirectory{EnginePath: "/var/opt/mssql/backup"}},
		{"only the plugin's half", &fwv1.SharedDirectory{LocalPath: "/mnt/backup"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := requireShare(&fwv1.Credentials{Host: "prod-1", Port: 1433, SharedDirectory: tc.share})
			if err == nil {
				t.Fatal("a backup with nowhere to write its file was accepted")
			}
			if !strings.Contains(err.Error(), "shared directory") {
				t.Errorf("the message does not explain what is missing: %v", err)
			}
		})
	}

	share := &fwv1.SharedDirectory{EnginePath: "/var/opt/mssql/backup", LocalPath: "/mnt/backup"}
	if _, err := requireShare(&fwv1.Credentials{SharedDirectory: share}); err != nil {
		t.Errorf("a complete shared directory was refused: %v", err)
	}
}
