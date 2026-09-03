package backup

import (
	"strings"
	"testing"
	"time"
)

// TestProtectionReasonExplainsEveryWayABackupSurvives.
//
// The reasons are asserted rather than eyeballed because they are the entire operational surface of
// the floor. There is no job row behind a sweep (ADR-0030), so "why is that one still there" is
// answered by this sentence or by nothing at all, and a reason that merely said "protected" would
// send an operator to read the source.
func TestProtectionReasonExplainsEveryWayABackupSurvives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                             string
		floorRecent, floorVerified, busy bool
		minKeep                          int
		wantEmpty                        bool
		wantContains                     []string
	}{
		{
			name:         "the most recent successful backup",
			floorRecent:  true,
			minKeep:      1,
			wantContains: []string{"it is this instance's most recent successful backup", "however old it is"},
		},
		{
			name:         "one of several the floor keeps",
			floorRecent:  true,
			minKeep:      3,
			wantContains: []string{"among this instance's 3 most recent successful backups"},
		},
		{
			name:          "the last one proven restorable",
			floorVerified: true,
			minKeep:       1,
			wantContains:  []string{"proven restorable", "last proof"},
		},
		{
			name:          "both rules at once, which is the common case on a healthy instance",
			floorRecent:   true,
			floorVerified: true,
			minKeep:       1,
			wantContains:  []string{"most recent successful backup", "proven restorable"},
		},
		{
			// Distinct from the floor on purpose: this one is temporary and the reader should not
			// be told it will never be deleted.
			name:         "something is reading it right now",
			busy:         true,
			minKeep:      1,
			wantContains: []string{"right now", "eligible again"},
		},
		{
			name:      "nothing protects it",
			minKeep:   1,
			wantEmpty: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := protectionReason(tc.floorRecent, tc.floorVerified, tc.busy, tc.minKeep)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("an unprotected backup reported a reason: %q", got)
				}
				return
			}
			if got == "" {
				t.Fatal("a protected backup reported no reason, so nothing explains why it survived")
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("reason %q does not mention %q", got, want)
				}
			}
		})
	}
}

// TestTheFloorOutranksTheConcurrencyGuardInWhatTheReaderIsTold.
//
// A backup that is both the last good one and currently being verified is permanently safe, not
// temporarily safe, and telling the reader the temporary reason would imply it disappears in a few
// minutes.
func TestTheFloorOutranksTheConcurrencyGuardInWhatTheReaderIsTold(t *testing.T) {
	t.Parallel()

	got := protectionReason(true, false, true, 1)
	if strings.Contains(got, "right now") {
		t.Fatalf("a backup the floor keeps was explained as a passing condition: %q", got)
	}
}

// TestASweepRefusesAPolicyThatRemovesItsOwnLimits is the second half of the check that
// config.Validate performs.
//
// Both halves are deliberate. The configuration is validated at startup so an operator learns
// immediately, and the sweep refuses again at the point of action so that a policy assembled in
// code — by a test, by a future caller, by a mistake — cannot delete an instance's last backup
// because a field defaulted to zero.
func TestASweepRefusesAPolicyThatRemovesItsOwnLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy RetentionPolicy
	}{
		{"no floor", RetentionPolicy{Enabled: true, MinKeep: 0, MaxPerSweep: 100}},
		{"no ceiling", RetentionPolicy{Enabled: true, MinKeep: 1, MaxPerSweep: 0}},
		{"the zero value, which is what a forgotten field looks like", RetentionPolicy{Enabled: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := &Service{retention: tc.policy}
			if _, err := svc.SweepRetention(testTenantCtx()); err == nil {
				t.Fatal("the sweep accepted a policy with no limits")
			}
		})
	}
}

// TestADisabledSweepDoesNothingAndSaysSo. A disabled sweep must not reach the database at all —
// which this asserts by giving the service no pool: anything that touched one would panic.
func TestADisabledSweepDoesNothingAndSaysSo(t *testing.T) {
	t.Parallel()

	svc := &Service{retention: RetentionPolicy{Enabled: false, MinKeep: 1, MaxPerSweep: 500}}
	result, err := svc.SweepRetention(testTenantCtx())
	if err != nil {
		t.Fatalf("a disabled sweep reported an error: %v", err)
	}
	if !result.Empty() {
		t.Fatalf("a disabled sweep reported work: %+v", result)
	}
}

// TestRetentionResultEmptyCountsEveryOutcome. Empty decides whether a sweep writes a log line, and
// a sweep that could not reach the object store all week must not be silent.
func TestRetentionResultEmptyCountsEveryOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result RetentionResult
		want   bool
	}{
		{"nothing happened", RetentionResult{}, true},
		{"something expired", RetentionResult{Expired: 1}, false},
		{"an artifact went", RetentionResult{ArtifactsDeleted: 1}, false},
		{"the store refused", RetentionResult{Unreachable: 1}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.result.Empty(); got != tc.want {
				t.Fatalf("Empty() = %v, want %v for %+v", got, tc.want, tc.result)
			}
		})
	}
}

// TestAnExpiryIsStampedFromTheRetentionTheRunWasGiven pins the arithmetic behind ADR-0031: a
// retention of N days becomes an expiry N days after the backup finished, and a retention of zero
// becomes no expiry at all.
//
// The second half is the one that matters. Zero is what every manual backup carries, and it is what
// every backup taken before this slice carries — so "zero stamps nothing" is the reason upgrading
// to this version deletes nothing.
func TestAnExpiryIsStampedFromTheRetentionTheRunWasGiven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		retentionDays int32
		wantStamped   bool
		wantAfter     time.Duration
	}{
		{"a week", 7, true, 7 * 24 * time.Hour},
		{"the schedule default", 30, true, 30 * 24 * time.Hour},
		{"no retention means no expiry, and no expiry means never deleted", 0, false, 0},
		{"a negative value cannot stamp a date in the past", -5, false, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			before := time.Now().UTC()
			got := stampedExpiry(tc.retentionDays, before)

			if !tc.wantStamped {
				if got != nil {
					t.Fatalf("a retention of %d stamped an expiry of %v", tc.retentionDays, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("a retention of %d days stamped no expiry", tc.retentionDays)
			}
			want := before.Add(tc.wantAfter)
			if diff := got.Sub(want); diff > time.Minute || diff < -time.Minute {
				t.Fatalf("expiry %v is %v away from the expected %v", *got, diff, want)
			}
		})
	}
}
