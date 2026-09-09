package config

import (
	"strings"
	"testing"
	"time"
)

// TestRetentionRefusesAConfigurationThatCouldDeleteEverything covers the three refusals that make
// retention safe to leave enabled.
//
// They are asserted rather than merely written because each one removes a way to configure the only
// part of Fleetward that destroys something into being unbounded, and a validation nobody tests is
// a validation somebody deletes. In particular, a floor of zero would permit retention to delete an
// instance's last working backup — which is not an exotic failure but the ordinary consequence of a
// correct "delete anything older than N days" (ADR-0032).
func TestRetentionRefusesAConfigurationThatCouldDeleteEverything(t *testing.T) {
	t.Parallel()

	base := func() RetentionConfig {
		return RetentionConfig{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500}
	}

	tests := []struct {
		name    string
		mutate  func(*RetentionConfig)
		wantErr string
	}{
		{
			name:   "a floor of zero is refused",
			mutate: func(r *RetentionConfig) { r.MinKeep = 0 },
			// The message has to say what is lost, not just that the number is wrong: an operator
			// setting this to zero is trying to reclaim storage and does not know what it costs.
			wantErr: "last successful backup",
		},
		{
			name:    "a negative floor is refused",
			mutate:  func(r *RetentionConfig) { r.MinKeep = -1 },
			wantErr: "RETENTION_MIN_KEEP",
		},
		{
			name:    "an unbounded sweep is refused",
			mutate:  func(r *RetentionConfig) { r.MaxPerSweep = 0 },
			wantErr: "RETENTION_MAX_PER_SWEEP",
		},
		{
			name:    "a zero interval is refused",
			mutate:  func(r *RetentionConfig) { r.Interval = 0 },
			wantErr: "RETENTION_INTERVAL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			cfg.Retention = base()
			tc.mutate(&cfg.Retention)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s: accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("the error must name what is wrong; got %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestRetentionIsRefusedEvenWhileDisabled pins a decision that is easy to get backwards.
//
// A dangerous value is rejected when it is written, not months later when somebody flips
// RETENTION_ENABLED on and discovers at 02:00 that the floor was zero all along.
func TestRetentionIsRefusedEvenWhileDisabled(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Retention = RetentionConfig{Enabled: false, Interval: time.Hour, MinKeep: 0, MaxPerSweep: 500}

	if err := cfg.Validate(); err == nil {
		t.Fatal("a zero floor was accepted because the sweep happened to be disabled")
	}
}

// TestDefaultRetentionIsValid is the other half: the shipped defaults must pass their own checks,
// or `docker compose up` refuses to start.
func TestDefaultRetentionIsValid(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Retention = RetentionConfig{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default retention configuration does not validate: %v", err)
	}
}

// validConfig is the smallest configuration that passes Validate, so that a test asserting one
// refusal is not also fighting an unrelated one.
func validConfig() *Config {
	return &Config{
		Environment: EnvDevelopment,
		Log:         LogConfig{Level: "info", Format: "json"},
		MetaDB:      MetaDBConfig{DSN: "postgres://localhost/fleetward"},
		Secrets:     SecretsConfig{Provider: "aesgcm", MasterKey: "not-a-real-key"},
		Scheduler: SchedulerConfig{
			Enabled: true, LeaseTTL: 2 * time.Minute, LeaseHeartbeat: 30 * time.Second,
		},
		Retention: RetentionConfig{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500},
		Alerts: AlertsConfig{
			Enabled: true, EvalInterval: 30 * time.Second,
			DeliveryWorkers: 2, DeliveryQueueSize: 256,
			DeliveryTimeout: 15 * time.Second, DeliveryAttempts: 3,
		},
	}
}

// TestDefaultAlertsConfigIsValid is the same guard the retention one above is: the shipped defaults
// have to pass their own checks, or `docker compose up` refuses to start on a fresh clone.
func TestDefaultAlertsConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default alerting configuration does not validate: %v", err)
	}
}

// TestAlertLimitsAreCheckedEvenWhenAlertingIsOff mirrors retention's rule, for its reason: a value
// that would be wrong when somebody turns alerting on should be refused when they write it, not
// months later when they flip the switch.
func TestAlertLimitsAreCheckedEvenWhenAlertingIsOff(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Alerts.Enabled = false
	cfg.Alerts.DeliveryWorkers = 0

	if err := cfg.Validate(); err == nil {
		t.Fatal("a delivery worker count of zero was accepted because alerting happened to be off")
	}
}
