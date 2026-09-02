package backup

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// TestPreviousRun pins the mirror of the scheduler's nextRun.
//
// Adherence asks what should already have happened, which is the question robfig/cron does not
// answer, so this walk is written here. The cases that matter are the ones where the expanding
// search could get it wrong: an expression that fires far more often than the first window, and one
// that fires far less often than it.
func TestPreviousRun(t *testing.T) {
	mustParse := func(t *testing.T, value string) time.Time {
		t.Helper()
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		return parsed
	}

	tests := []struct {
		name     string
		cron     string
		timezone string
		at       string
		want     string
	}{
		{
			name: "nightly, asked in the afternoon",
			cron: "0 2 * * *", timezone: "UTC",
			at: "2026-09-02T15:04:05Z", want: "2026-09-02T02:00:00Z",
		},
		{
			name: "nightly, asked before it has fired today",
			cron: "0 2 * * *", timezone: "UTC",
			at: "2026-09-02T01:00:00Z", want: "2026-09-01T02:00:00Z",
		},
		{
			name: "every five minutes, where a wide first window would iterate forever",
			cron: "*/5 * * * *", timezone: "UTC",
			at: "2026-09-02T15:04:05Z", want: "2026-09-02T15:00:00Z",
		},
		{
			name: "monthly, where a narrow window finds nothing and has to widen",
			cron: "0 3 1 * *", timezone: "UTC",
			at: "2026-09-20T09:00:00Z", want: "2026-09-01T03:00:00Z",
		},
		{
			// The reason the computation happens in the schedule's own location. 02:00 in Bucharest
			// is 23:00 UTC the previous day in summer, and a DBA who wrote "0 2 * * *" meant the
			// local hour, not the UTC one.
			name: "a local hour is a different UTC instant in summer",
			cron: "0 2 * * *", timezone: "Europe/Bucharest",
			at: "2026-07-15T12:00:00Z", want: "2026-07-14T23:00:00Z",
		},
		{
			name: "the same local hour in winter",
			cron: "0 2 * * *", timezone: "Europe/Bucharest",
			at: "2026-01-15T12:00:00Z", want: "2026-01-15T00:00:00Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := previousRun(tt.cron, tt.timezone, mustParse(t, tt.at))
			if err != nil {
				t.Fatalf("previousRun: %v", err)
			}
			if want := mustParse(t, tt.want); !got.Equal(want) {
				t.Errorf("previousRun = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

func TestPreviousRunRejectsWhatCannotBeEvaluated(t *testing.T) {
	tests := []struct{ name, cron, timezone string }{
		{"a cron expression that does not parse", "not a cron", "UTC"},
		{"a timezone that is not in the database", "0 2 * * *", "Mars/Olympus_Mons"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := previousRun(tt.cron, tt.timezone, time.Now()); err == nil {
				t.Fatal("want an error, got none")
			}
		})
	}
}

// TestEvaluateWindowWaitsForTheGrace covers the mistake that would make this feature useless.
//
// An instance expected to back up at 02:00 with two hours of grace must not be reported as behind at
// 02:30 while the backup is still running. Until the grace has run out there is nothing to answer
// yet, so the occurrence under judgement is still the previous one.
func TestEvaluateWindowWaitsForTheGrace(t *testing.T) {
	at := func(value string) time.Time {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		return parsed
	}

	tests := []struct {
		name string
		now  string
		want string
	}{
		{"half an hour in, with two hours of grace", "2026-09-02T02:30:00Z", "2026-09-01T02:00:00Z"},
		{"one minute before the grace runs out", "2026-09-02T03:59:00Z", "2026-09-01T02:00:00Z"},
		{"one minute after it runs out", "2026-09-02T04:01:00Z", "2026-09-02T02:00:00Z"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &expectation{cron: "0 2 * * *", timezone: "UTC", graceMinutes: 120}
			if err := e.evaluateWindow(at(tt.now)); err != nil {
				t.Fatalf("evaluateWindow: %v", err)
			}
			if want := at(tt.want); !e.expectedBy.Equal(want) {
				t.Errorf("expectedBy = %s, want %s",
					e.expectedBy.Format(time.RFC3339), want.Format(time.RFC3339))
			}
			if want := e.expectedBy.Add(2 * time.Hour); !e.deadline.Equal(want) {
				t.Errorf("deadline = %s, want %s",
					e.deadline.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// TestDecide is where the product's central distinction is enforced: a backup that arrived is not
// the same as a backup that worked, and evidence that cannot say which is not permitted to look
// like either.
func TestDecide(t *testing.T) {
	window := func() *expectation {
		return &expectation{
			expectedBy: time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC),
			deadline:   time.Date(2026, 9, 2, 4, 0, 0, 0, time.UTC),
		}
	}
	backup := func(state fwv1.BackupState, at time.Time, approximate bool) *fwv1.Backup {
		b := &fwv1.Backup{State: state, CompletedAt: timestamppb.New(at)}
		if approximate {
			b.Evidence = &fwv1.ObservedEvidence{CompletedAtIsApproximate: true}
		}
		return b
	}

	inside := time.Date(2026, 9, 2, 2, 30, 0, 0, time.UTC)
	justOutside := time.Date(2026, 9, 2, 4, 30, 0, 0, time.UTC)
	wellOutside := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		candidates []*fwv1.Backup
		want       fwv1.AdherenceState
	}{
		{
			name: "nothing arrived",
			want: fwv1.AdherenceState_ADHERENCE_STATE_MISSED,
		},
		{
			name:       "a backup succeeded inside the window",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, inside, false)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT,
		},
		{
			name:       "only evidence that cannot report an outcome",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_UNKNOWN, inside, false)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_UNPROVEN,
		},
		{
			name:       "the backup ran and failed",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_FAILED, inside, false)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_FAILED,
		},
		{
			// A failure that was retried and worked is an estate behaving, not an estate at risk.
			name: "a failure and a success in the same window",
			candidates: []*fwv1.Backup{
				backup(fwv1.BackupState_BACKUP_STATE_FAILED, inside, false),
				backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, inside, false),
			},
			want: fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT,
		},
		{
			// A success outranks unproven evidence, whatever order they arrive in.
			name: "unproven evidence and a proven backup",
			candidates: []*fwv1.Backup{
				backup(fwv1.BackupState_BACKUP_STATE_UNKNOWN, inside, false),
				backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, inside, false),
			},
			want: fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT,
		},
		{
			// The daylight-saving allowance, and the point is that it is not free: only a record
			// whose timestamp the plugin admitted it could not pin down gets the extra hour.
			name:       "half an hour late, with an exact timestamp",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, justOutside, false)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_MISSED,
		},
		{
			name:       "half an hour late, with a timestamp that may be out by an hour",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, justOutside, true)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT,
		},
		{
			name:       "four hours late, even with the allowance",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_SUCCEEDED, wellOutside, true)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_MISSED,
		},
		{
			// A backup still running is not a backup that happened.
			name:       "a run that never finished",
			candidates: []*fwv1.Backup{backup(fwv1.BackupState_BACKUP_STATE_RUNNING, inside, false)},
			want:       fwv1.AdherenceState_ADHERENCE_STATE_MISSED,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := window()
			e.candidates = tt.candidates
			e.decide()
			if e.state != tt.want {
				t.Errorf("state = %s, want %s", e.state, tt.want)
			}
			if tt.want == fwv1.AdherenceState_ADHERENCE_STATE_MISSED && e.satisfied != nil {
				t.Error("a missed window reports a backup that satisfied it")
			}
		})
	}
}

// TestCaveatsFor asserts that every weakness in an answer is stated, and that an answer resting on
// a backup Fleetward took states nothing — a caveat on a managed backup would be noise, and noise
// is how a real caveat stops being read.
func TestCaveatsFor(t *testing.T) {
	tests := []struct {
		name     string
		backup   *fwv1.Backup
		wantSome bool
	}{
		{
			name:   "a backup Fleetward took",
			backup: &fwv1.Backup{Origin: fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED},
		},
		{
			name:   "no backup at all",
			backup: nil,
		},
		{
			name: "evidence that proves everything it can",
			backup: &fwv1.Backup{Evidence: &fwv1.ObservedEvidence{
				SourceDescription:        "the engine's own record",
				ReportsOutcome:           true,
				IdentityIsEngineAssigned: true,
			}},
		},
		{
			name: "a directory listing",
			backup: &fwv1.Backup{Evidence: &fwv1.ObservedEvidence{
				SourceDescription: "a configured backup directory",
			}},
			wantSome: true,
		},
		{
			name: "an approximate finish time",
			backup: &fwv1.Backup{Evidence: &fwv1.ObservedEvidence{
				SourceDescription:        "the engine's own record",
				ReportsOutcome:           true,
				IdentityIsEngineAssigned: true,
				CompletedAtIsApproximate: true,
			}},
			wantSome: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := caveatsFor(tt.backup)
			if tt.wantSome && len(got) == 0 {
				t.Error("no caveat was reported for evidence that cannot prove what it is used for")
			}
			if !tt.wantSome && len(got) > 0 {
				t.Errorf("caveats reported where there is nothing to warn about: %v", got)
			}
		})
	}
}
