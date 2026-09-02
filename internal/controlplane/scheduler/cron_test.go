package scheduler

import (
	"errors"
	"testing"
	"time"

	// The time zone database, embedded so this test does not depend on the host having one. It is
	// the same import the control plane's main carries, and for the same reason: without it the
	// suite passes on a developer's machine and fails in a container.
	_ "time/tzdata"
)

func TestParseCron(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{name: "nightly", expr: "0 2 * * *"},
		{name: "every minute", expr: "* * * * *"},
		{name: "weekly", expr: "30 3 * * 0"},
		{name: "descriptor", expr: "@daily"},
		{name: "empty", expr: "", wantErr: true},
		{name: "too few fields", expr: "0 2 *", wantErr: true},
		{name: "not a cron expression at all", expr: "nightly please", wantErr: true},
		{name: "hour out of range", expr: "0 99 * * *", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseCron(tc.expr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCron(%q) succeeded; want an error", tc.expr)
				}
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("parseCron(%q) error = %v; want ErrInvalidArgument so the API answers 400", tc.expr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCron(%q) = %v", tc.expr, err)
			}
		})
	}
}

func TestLoadLocation(t *testing.T) {
	t.Parallel()

	if loc, err := loadLocation(""); err != nil || loc != time.UTC {
		t.Fatalf("loadLocation(\"\") = %v, %v; an unset timezone must mean UTC", loc, err)
	}
	if _, err := loadLocation("Europe/Bucharest"); err != nil {
		t.Fatalf("loadLocation(\"Europe/Bucharest\") = %v; the embedded tzdata should make this work anywhere", err)
	}
	err := func() error { _, err := loadLocation("Middle/Earth"); return err }()
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("loadLocation of an unknown zone = %v; want ErrInvalidArgument", err)
	}
}

// TestNextRunUsesTheScheduleLocation is the reason timezone is a column rather than an assumption.
//
// A DBA writing "0 2 * * *" for a Bucharest server means 02:00 where the server is. That is 00:00
// UTC in winter and 23:00 UTC the previous day in summer. Computing in UTC instead would walk the
// backup window an hour into and out of the business day twice a year.
func TestNextRunUsesTheScheduleLocation(t *testing.T) {
	t.Parallel()

	bucharest, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatalf("load Europe/Bucharest: %v", err)
	}

	tests := []struct {
		name     string
		after    time.Time
		wantUTC  string
		timezone string
	}{
		{
			name:     "winter, EET is UTC+2",
			after:    time.Date(2026, 1, 15, 12, 0, 0, 0, bucharest),
			timezone: "Europe/Bucharest",
			wantUTC:  "2026-01-16T00:00:00Z",
		},
		{
			name:     "summer, EEST is UTC+3",
			after:    time.Date(2026, 7, 15, 12, 0, 0, 0, bucharest),
			timezone: "Europe/Bucharest",
			wantUTC:  "2026-07-15T23:00:00Z",
		},
		{
			name:     "an unset timezone is UTC, so 02:00 is 02:00",
			after:    time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
			timezone: "",
			wantUTC:  "2026-07-16T02:00:00Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := nextRun("0 2 * * *", tc.timezone, tc.after)
			if err != nil {
				t.Fatalf("nextRun: %v", err)
			}
			if got.Format(time.RFC3339) != tc.wantUTC {
				t.Fatalf("nextRun = %s; want %s", got.Format(time.RFC3339), tc.wantUTC)
			}
			if got.Location() != time.UTC {
				t.Fatalf("nextRun returned %s; the stored value is always UTC", got.Location())
			}
		})
	}
}

// TestNextRunAcrossDaylightSaving pins what a transition does, rather than pretending it does not
// happen.
//
// Europe/Bucharest springs forward on 2026-03-29 (03:00 local becomes 04:00; 03:30 does not exist)
// and falls back on 2026-10-25 (04:00 local becomes 03:00; 03:30 happens twice). A schedule at
// either hour has an answer, and this test is where that answer is written down — so that a
// dependency upgrade changing it fails here rather than in someone's estate. The behaviour is
// documented in docs/ops/scheduling.md.
func TestNextRunAcrossDaylightSaving(t *testing.T) {
	t.Parallel()

	const tz = "Europe/Bucharest"
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load %s: %v", tz, err)
	}

	tests := []struct {
		name    string
		expr    string
		after   time.Time
		wantUTC string
		why     string
	}{
		{
			name:    "spring forward: an hour that does not exist is skipped to the next day",
			expr:    "30 3 * * *",
			after:   time.Date(2026, 3, 28, 4, 0, 0, 0, loc),
			wantUTC: "2026-03-30T00:30:00Z",
			why:     "03:30 local does not exist on 2026-03-29, so the run lands on the 30th at 03:30 EEST",
		},
		{
			name:    "spring forward: an hour before the gap is unaffected",
			expr:    "30 1 * * *",
			after:   time.Date(2026, 3, 28, 4, 0, 0, 0, loc),
			wantUTC: "2026-03-28T23:30:00Z",
			why:     "01:30 on the 29th is still EET, UTC+2",
		},
		{
			name:    "fall back: the first of the two 03:30s fires",
			expr:    "30 3 * * *",
			after:   time.Date(2026, 10, 24, 12, 0, 0, 0, loc),
			wantUTC: "2026-10-25T00:30:00Z",
			why:     "03:30 EEST, the first occurrence, before the clock repeats the hour",
		},
		{
			name:    "fall back: the run after it is the following day, not the repeat",
			expr:    "30 3 * * *",
			after:   time.Date(2026, 10, 25, 3, 30, 0, 0, loc),
			wantUTC: "2026-10-26T01:30:00Z",
			why:     "the repeated 03:30 EET is not fired again; the next run is the 26th",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := nextRun(tc.expr, tz, tc.after)
			if err != nil {
				t.Fatalf("nextRun: %v", err)
			}
			if got.Format(time.RFC3339) != tc.wantUTC {
				t.Fatalf("nextRun(%q) after %s = %s; want %s\n%s",
					tc.expr, tc.after.Format(time.RFC3339), got.Format(time.RFC3339), tc.wantUTC, tc.why)
			}
		})
	}
}

// TestNextRunAlwaysMovesForward is the property the tick loop depends on: an advanced schedule must
// have a next_run_at strictly in the future, or the loop would fire it again on the very next tick
// and turn one schedule into a hot loop against a production server.
func TestNextRunAlwaysMovesForward(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	for _, expr := range []string{"* * * * *", "0 2 * * *", "30 3 * * *", "@hourly"} {
		for _, tz := range []string{"UTC", "Europe/Bucharest", "America/Santiago", "Australia/Lord_Howe"} {
			got, err := nextRun(expr, tz, now)
			if err != nil {
				t.Fatalf("nextRun(%q, %q): %v", expr, tz, err)
			}
			if !got.After(now) {
				t.Fatalf("nextRun(%q, %q) = %s, which is not after %s", expr, tz, got, now)
			}
		}
	}
}
