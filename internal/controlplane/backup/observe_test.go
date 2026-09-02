package backup

import (
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// TestObservedState pins the mapping that decides what a DBA sees.
//
// The one that matters is UNKNOWN, and it matters because rounding it either way is a lie: rounded
// up it reports a backup nobody has checked as a success, and rounded down it reports a healthy
// estate as failing. It is neither, and this product exists because that distinction is the whole
// game (ADR-0015).
func TestObservedState(t *testing.T) {
	tests := []struct {
		name    string
		outcome fwv1.ObservedOutcome
		want    string
	}{
		{"the engine recorded a completed backup", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED, "succeeded"},
		{"the engine recorded a failure", fwv1.ObservedOutcome_OBSERVED_OUTCOME_FAILED, "failed"},
		{"the evidence cannot say", fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN, "unknown"},
		{"a plugin that left the field unset", fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNSPECIFIED, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := observedState(&fwv1.ObservedBackup{Outcome: tt.outcome})
			if got != tt.want {
				t.Errorf("observedState = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestObservedMethodIsNeverEmpty(t *testing.T) {
	if got := observedMethod(&fwv1.ObservedBackup{Method: "log"}); got != "log" {
		t.Errorf("observedMethod = %q, want %q", got, "log")
	}
	// method_id is NOT NULL, and a plugin that reports no method must not fail the whole poll.
	if got := observedMethod(&fwv1.ObservedBackup{}); got == "" {
		t.Error("observedMethod returned an empty string, which the column will not take")
	}
}

// TestBackupStateRoundTrip asserts the two directions agree. They are used on opposite sides of the
// same query — one writes a filter, the other reads a row — so a disagreement would silently return
// nothing rather than fail.
func TestBackupStateRoundTrip(t *testing.T) {
	states := []fwv1.BackupState{
		fwv1.BackupState_BACKUP_STATE_PENDING,
		fwv1.BackupState_BACKUP_STATE_RUNNING,
		fwv1.BackupState_BACKUP_STATE_SUCCEEDED,
		fwv1.BackupState_BACKUP_STATE_FAILED,
		fwv1.BackupState_BACKUP_STATE_CANCELED,
		fwv1.BackupState_BACKUP_STATE_EXPIRED,
		fwv1.BackupState_BACKUP_STATE_UNKNOWN,
	}
	for _, state := range states {
		t.Run(state.String(), func(t *testing.T) {
			if got := parseBackupState(backupStateName(state)); got != state {
				t.Errorf("round trip gave %s, want %s", got, state)
			}
		})
	}
}

// TestParseBackupOrigin covers the default, which is the whole reason origin has one: every backup
// row written before this column existed was taken by Fleetward.
func TestParseBackupOrigin(t *testing.T) {
	tests := []struct {
		stored string
		want   fwv1.BackupOrigin
	}{
		{originManaged, fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED},
		{originObserved, fwv1.BackupOrigin_BACKUP_ORIGIN_OBSERVED},
		{"", fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED},
	}
	for _, tt := range tests {
		t.Run(tt.stored, func(t *testing.T) {
			if got := parseBackupOrigin(tt.stored); got != tt.want {
				t.Errorf("parseBackupOrigin(%q) = %s, want %s", tt.stored, got, tt.want)
			}
		})
	}
}

func TestPrefixedQualifiesEveryColumn(t *testing.T) {
	got := prefixed("id, instance_id,\n\t   state", "b.")
	want := "b.id, b.instance_id, b.state"
	if got != want {
		t.Errorf("prefixed = %q, want %q", got, want)
	}
}
