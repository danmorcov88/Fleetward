package sqlserver

import (
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// TestBackupTypeName renders msdb's one-character type code in the engine's own vocabulary. It is
// display only — core reads BackupKind — but a DBA reading "L" where they expected "log" is a DBA
// checking the wrong thing during an incident.
func TestBackupTypeName(t *testing.T) {
	tests := []struct{ code, want string }{
		{"D", "database"},
		{"I", "differential"},
		{"L", "log"},
		{"F", "file"},
		{"G", "differential file"},
		{"P", "partial"},
		{"Q", "differential partial"},
		{"d", "database"},
		{" L ", "log"},
		{"Z", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := backupTypeName(tt.code); got != tt.want {
				t.Errorf("backupTypeName(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// TestDamagedOutcome covers the one case where this source can report a failure.
//
// A row exists in the engine's backup history only when a backup completed, so its presence is
// evidence of success and a backup that failed leaves no row at all — which surfaces as a window
// nothing satisfied, the answer a DBA wants. is_damaged is the remaining case: the engine wrote the
// set anyway, over a database it had already found damage in, which is evidence about the artifact.
func TestDamagedOutcome(t *testing.T) {
	if got := damagedOutcome(false); got != fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED {
		t.Errorf("an undamaged backup set reads as %s, want SUCCEEDED", got)
	}
	if got := damagedOutcome(true); got != fwv1.ObservedOutcome_OBSERVED_OUTCOME_FAILED {
		t.Errorf("a damaged backup set reads as %s, want FAILED", got)
	}
}

// TestServerClockExactness is what decides whether a record's timestamp is trusted as written or
// widened by an hour when it is compared to a compliance window.
func TestServerClockExactness(t *testing.T) {
	if !(serverClockInfo{timezone: "Central European Standard Time"}).exact() {
		t.Error("an instance that names its own time zone converts exactly")
	}
	// An instance too old to name its zone can only offer the offset in force right now, which is
	// wrong by one daylight-saving transition for a backup on the other side of one.
	if (serverClockInfo{offset: 3 * 60 * 1e9}).exact() {
		t.Error("an offset alone is not an exact conversion")
	}
}

func TestNonEmpty(t *testing.T) {
	got := nonEmpty([]string{"app", "", "  ", " reporting "})
	if len(got) != 2 || got[0] != "app" || got[1] != "reporting" {
		t.Errorf("nonEmpty = %#v, want [app reporting]", got)
	}
	// An empty database list means every database, and a list of blanks must not become a filter
	// that matches none of them.
	if len(nonEmpty([]string{"", " "})) != 0 {
		t.Error("a list of blanks should produce no filter at all")
	}
}

// TestBackupHistoryCapabilitiesAreHonest guards the matrix core trusts when it decides what to say
// about somebody's estate.
func TestBackupHistoryCapabilitiesAreHonest(t *testing.T) {
	caps := backupHistoryCapabilities()
	if !caps.GetSupported() {
		t.Fatal("this plugin implements ListBackupHistory and must declare it")
	}
	if caps.GetSourceDescription() == "" {
		t.Error("a report has to be able to say what it read")
	}
	if !caps.GetIdentityIsEngineAssigned() {
		t.Error("the engine assigns the backup set's identity, and it survives the file being moved")
	}
	if !caps.GetReportsOutcome() {
		t.Error("a row in the engine's backup history is written only when a backup completed")
	}
	if caps.GetRequiresSharedDirectory() {
		t.Error("the history is read over the connection; no filesystem is involved")
	}
}
