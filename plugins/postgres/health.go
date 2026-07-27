package postgres

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

// defaultHealthTimeout bounds a health probe when the request supplies none.
const defaultHealthTimeout = 10 * time.Second

// Health thresholds. These are starting points, not tuned values; once instances carry their own
// policy the numbers move into configuration rather than into a fork of this logic.
const (
	// connectionUsageWarn is the fraction of max_connections above which the instance is degraded.
	// Running out of connections takes an instance down for every client at once, and it arrives
	// with almost no warning, so the alarm is deliberately early.
	connectionUsageWarn = 0.85
	// replicationLagWarn is how far a replica may fall behind before the instance is degraded.
	replicationLagWarn = 5 * time.Minute
)

// HealthCheck probes the instance and reports its state.
//
// An unreachable instance returns HEALTH_STATE_DOWN with a populated error, not a gRPC failure.
// The contract is explicit about this, and the reason is operational: "down" is the single most
// important answer this RPC gives. Returning it as an RPC error would make the manager treat a
// correctly functioning plugin as broken and would lose the distinction between "the database is
// down" and "we could not ask".
func (p *Plugin) HealthCheck(ctx context.Context, req *fwv1.HealthCheckRequest) (*fwv1.HealthStatus, error) {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	conn, err := connect(ctx, req.GetCredentials())
	if err != nil {
		return down(err, time.Since(start)), nil
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	// The liveness probe proper: a trivial query that still exercises the full round trip.
	var one int
	if err := conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		return down(sdk.ConnectionFailed("health query failed").WithCause(err), time.Since(start)), nil
	}
	latency := time.Since(start)

	status := &fwv1.HealthStatus{
		State:   fwv1.HealthState_HEALTH_STATE_UP,
		Message: "accepting connections",
		Latency: durationpb.New(latency),
	}

	var serverVersion string
	if err := conn.QueryRow(ctx, `SHOW server_version`).Scan(&serverVersion); err == nil {
		status.EngineVersion = normalizeVersion(serverVersion)
	}

	// Signals are collected best-effort. A monitoring account may lack the privilege for some of
	// these, and a missing signal must never downgrade a healthy instance to unhealthy — that would
	// turn a permissions problem into a false outage.
	status.Signals = collectSignals(ctx, conn)

	for _, signal := range status.GetSignals() {
		if signal.GetSeverity() == fwv1.Severity_SEVERITY_WARNING ||
			signal.GetSeverity() == fwv1.Severity_SEVERITY_CRITICAL {
			status.State = fwv1.HealthState_HEALTH_STATE_DEGRADED
			status.Message = signal.GetMessage()
			break
		}
	}

	return status, nil
}

// down builds the response for an instance that could not be reached.
func down(err error, latency time.Duration) *fwv1.HealthStatus {
	pluginErr := sdk.AsPluginError(err)
	return &fwv1.HealthStatus{
		State:   fwv1.HealthState_HEALTH_STATE_DOWN,
		Message: pluginErr.GetMessage(),
		Latency: durationpb.New(latency),
		Error:   pluginErr,
	}
}

// collectSignals gathers health indicators beyond simple reachability.
func collectSignals(ctx context.Context, conn queryer) []*fwv1.HealthSignal {
	var signals []*fwv1.HealthSignal

	if s := connectionSignal(ctx, conn); s != nil {
		signals = append(signals, s)
	}
	if s := recoverySignal(ctx, conn); s != nil {
		signals = append(signals, s)
	}
	if s := replicationLagSignal(ctx, conn); s != nil {
		signals = append(signals, s)
	}
	return signals
}

// connectionSignal reports how close the instance is to exhausting max_connections.
func connectionSignal(ctx context.Context, conn queryer) *fwv1.HealthSignal {
	var used, limit int64
	err := conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pg_stat_activity),
		       current_setting('max_connections')::bigint`).Scan(&used, &limit)
	if err != nil || limit <= 0 {
		return nil
	}

	usage := float64(used) / float64(limit)
	severity := fwv1.Severity_SEVERITY_INFO
	message := "connection usage is normal"
	if usage >= connectionUsageWarn {
		severity = fwv1.Severity_SEVERITY_WARNING
		message = "connection usage is close to max_connections"
	}

	return &fwv1.HealthSignal{
		Name:     "connection_usage",
		Severity: severity,
		Message:  message,
		Value:    usage * 100,
		Unit:     "%",
	}
}

// recoverySignal reports whether the instance is a standby.
//
// This is informational, never a warning: a replica in recovery is entirely normal. It exists
// because "why is this instance read-only" is a question an operator asks often, and because
// backup and restore decisions depend on the answer.
func recoverySignal(ctx context.Context, conn queryer) *fwv1.HealthSignal {
	var inRecovery bool
	if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return nil
	}
	if !inRecovery {
		return nil
	}
	return &fwv1.HealthSignal{
		Name:     "in_recovery",
		Severity: fwv1.Severity_SEVERITY_INFO,
		Message:  "instance is a standby in recovery",
		Value:    1,
		Unit:     "1",
	}
}

// replicationLagSignal reports how far behind a standby is.
func replicationLagSignal(ctx context.Context, conn queryer) *fwv1.HealthSignal {
	var inRecovery bool
	if err := conn.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil || !inRecovery {
		return nil
	}

	var lagSeconds *float64
	err := conn.QueryRow(ctx, `
		SELECT CASE
		         WHEN pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn() THEN 0
		         ELSE EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp()))
		       END`).Scan(&lagSeconds)
	if err != nil || lagSeconds == nil {
		return nil
	}

	severity := fwv1.Severity_SEVERITY_INFO
	message := "replication lag is within tolerance"
	if *lagSeconds > replicationLagWarn.Seconds() {
		severity = fwv1.Severity_SEVERITY_WARNING
		message = "replica is lagging behind the primary"
	}

	return &fwv1.HealthSignal{
		Name:     "replication_lag",
		Severity: severity,
		Message:  message,
		Value:    *lagSeconds,
		Unit:     "s",
	}
}
