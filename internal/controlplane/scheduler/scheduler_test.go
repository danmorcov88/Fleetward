package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/danmorcov88/fleetward/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestOwnerIDIsUniquePerProcess is the reason lease_owner carries a random suffix rather than just
// <hostname>/<pid>.
//
// A control plane restarted on the same host can be handed a recycled pid. Without the suffix the
// new process would present the same owner string as the one that died holding a lease, and could
// renew — and therefore keep — a lease it never claimed.
func TestOwnerIDIsUniquePerProcess(t *testing.T) {
	t.Parallel()

	first, second := newOwnerID(), newOwnerID()
	if first == second {
		t.Fatalf("two owner identities are identical (%q); a recycled pid would impersonate a dead process", first)
	}
	for _, id := range []string{first, second} {
		if parts := strings.Split(id, "/"); len(parts) != 3 {
			t.Fatalf("owner %q is not <hostname>/<pid>/<uuid>", id)
		}
	}
}

// TestIntervalRendersSeconds guards a mistake that would be invisible until a lease behaved
// strangely: PostgreSQL reads a bare integer in `now() + $1` as microseconds, so passing a
// time.Duration straight through would turn a two-minute lease into a two-hour one.
func TestInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{name: "two minutes", in: 2 * time.Minute, want: "120.000000 seconds"},
		{name: "thirty seconds", in: 30 * time.Second, want: "30.000000 seconds"},
		{name: "sub-second", in: 1500 * time.Millisecond, want: "1.500000 seconds"},
		{name: "zero", in: 0, want: "0 seconds"},
		{name: "negative is not a lease", in: -time.Second, want: "0 seconds"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := interval(tc.in); got != tc.want {
				t.Fatalf("interval(%s) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestShouldVerify checks the policy that decides whether a successful backup is proven restorable.
//
// The default direction matters more than the arithmetic: an unset or unrecognised policy verifies.
// A backup that looks verified and is not is the exact state this product exists to eliminate, so
// the failure mode of this function is "verified something it did not have to".
func TestShouldVerify(t *testing.T) {
	t.Parallel()

	s := &Scheduler{}
	tests := []struct {
		name    string
		payload jobPayload
		want    bool
	}{
		{name: "always", payload: jobPayload{VerifyPolicy: verifyAlways}, want: true},
		{name: "manual never verifies automatically", payload: jobPayload{VerifyPolicy: verifyManual}, want: false},
		{name: "unset defaults to verifying", payload: jobPayload{}, want: true},
		{name: "an unknown policy defaults to verifying", payload: jobPayload{VerifyPolicy: "whenever"}, want: true},
		{
			name:    "sampled at 100 always verifies",
			payload: jobPayload{VerifyPolicy: verifySampled, VerifySamplePercent: 100},
			want:    true,
		},
		{
			name:    "sampled at 0 never verifies",
			payload: jobPayload{VerifyPolicy: verifySampled, VerifySamplePercent: 0},
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := s.shouldVerify(tc.payload); got != tc.want {
				t.Fatalf("shouldVerify(%+v) = %v; want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// TestShouldVerifySamplingIsActuallyRandom checks that a partial sample is neither always nor never.
// A sampling policy that silently collapsed to one of those would be a schedule quietly not doing
// what its owner asked.
func TestShouldVerifySamplingIsActuallyRandom(t *testing.T) {
	t.Parallel()

	s := &Scheduler{}
	p := jobPayload{VerifyPolicy: verifySampled, VerifySamplePercent: 50}

	yes := 0
	const runs = 500
	for range runs {
		if s.shouldVerify(p) {
			yes++
		}
	}
	if yes == 0 || yes == runs {
		t.Fatalf("sampling at 50%% chose %d of %d; it has collapsed to a constant", yes, runs)
	}
}

// TestJobPayloadRoundTrip protects the snapshot a job carries. If a field stops surviving the round
// trip, a scheduled run silently loses the method or the verification policy it was created with.
func TestJobPayloadRoundTrip(t *testing.T) {
	t.Parallel()

	original := jobPayload{
		MethodID:            "pg_dump",
		Options:             map[string]string{"format": "custom"},
		VerifyPolicy:        verifySampled,
		VerifySamplePercent: 25,
		BackupID:            "5b6f5e2e-0000-4000-8000-000000000001",
	}

	raw, err := original.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodePayload(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MethodID != original.MethodID ||
		got.VerifyPolicy != original.VerifyPolicy ||
		got.VerifySamplePercent != original.VerifySamplePercent ||
		got.BackupID != original.BackupID ||
		got.Options["format"] != "custom" {
		t.Fatalf("payload round trip lost data: %+v", got)
	}

	// An empty payload is what a manually created job carries, and it must decode rather than fail.
	if _, err := decodePayload(nil); err != nil {
		t.Fatalf("decodePayload(nil) = %v; a job with no payload is legitimate", err)
	}
	if _, err := decodePayload([]byte("{")); err == nil {
		t.Fatal("decodePayload accepted malformed JSON")
	}
	if !json.Valid(raw) {
		t.Fatalf("encode produced invalid JSON: %s", raw)
	}
}

// TestHealthCheckReportsAStalledLoop is what turns "the scheduler stopped" from a silent failure
// into a degraded readiness probe. A loop that has quietly stopped looks exactly like an estate
// with nothing scheduled, which is why it is reported rather than inferred.
func TestHealthCheckReportsAStalledLoop(t *testing.T) {
	t.Parallel()

	cfg := config.SchedulerConfig{Enabled: true, PollInterval: 10 * time.Second}
	s := New(nil, nil, cfg, discardLogger())

	s.lastTick.Store(time.Now().UnixNano())
	if err := s.HealthCheck(context.Background()); err != nil {
		t.Fatalf("a scheduler that has just ticked is healthy; got %v", err)
	}

	s.lastTick.Store(time.Now().Add(-5 * time.Minute).UnixNano())
	err := s.HealthCheck(context.Background())
	if err == nil {
		t.Fatal("a scheduler that has not ticked in five minutes reported itself healthy")
	}
	if !strings.Contains(err.Error(), "nothing is running automatically") {
		t.Fatalf("the message must say what the operator loses; got %q", err)
	}

	// A scheduler that was deliberately switched off is not unhealthy, it is off.
	off := New(nil, nil, config.SchedulerConfig{Enabled: false}, discardLogger())
	off.lastTick.Store(time.Now().Add(-24 * time.Hour).UnixNano())
	if err := off.HealthCheck(context.Background()); err != nil {
		t.Fatalf("a disabled scheduler must not degrade readiness; got %v", err)
	}
}

// TestCloseIsSafeWhenDisabled covers the shutdown path of a control plane started with the
// scheduler switched off. Close must return rather than block on a loop that never started.
func TestCloseIsSafeWhenDisabled(t *testing.T) {
	t.Parallel()

	s := New(nil, nil, config.SchedulerConfig{Enabled: false}, discardLogger())
	s.Start(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on a scheduler that was never started")
	}
}

// TestEnumMappingRoundTrips guards the one place the database's vocabulary and the contract's
// vocabulary meet. A value that maps one way and not the other would show a job in the API as
// UNSPECIFIED while the row says otherwise.
func TestEnumMappingRoundTrips(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"backup", "verify", "restore", "discovery", "metrics"} {
		if got := jobKindName(jobKindFromName(name)); got != name {
			t.Fatalf("job kind %q round-tripped to %q", name, got)
		}
	}
	for _, name := range []string{"pending", "running", "succeeded", "failed", "canceled"} {
		if got := jobStateName(jobStateFromName(name)); got != name {
			t.Fatalf("job state %q round-tripped to %q", name, got)
		}
	}
	for _, name := range []string{verifyAlways, verifySampled, verifyManual} {
		if got := verifyPolicyName(verifyPolicyFromName(name)); got != name {
			t.Fatalf("verify policy %q round-tripped to %q", name, got)
		}
	}
	if jobKindName(jobKindFromName("nonsense")) != "" {
		t.Fatal("an unknown kind must map to the unspecified enum, not to a guess")
	}
}
