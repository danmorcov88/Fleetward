package sqlserver

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	// defaultHealthTimeout bounds a health probe when the request supplies none.
	defaultHealthTimeout = 10 * time.Second

	// connectionUsageWarn is the fraction of the configured connection limit above which the
	// instance is degraded. Running out of worker threads takes an instance down for every client
	// at once and arrives with almost no warning, so the alarm is deliberately early.
	connectionUsageWarn = 0.85
)

// HealthCheck probes the instance and reports its state.
//
// An unreachable instance returns HEALTH_STATE_DOWN with a populated error, not a gRPC failure.
// "Down" is the single most important answer this RPC gives; returning it as an RPC error would
// make the manager treat a correctly functioning plugin as broken, and would lose the distinction
// between "the database is down" and "we could not ask".
func (p *Plugin) HealthCheck(ctx context.Context, req *fwv1.HealthCheckRequest) (*fwv1.HealthStatus, error) {
	timeout := req.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	db, err := open(req.GetCredentials(), masterDatabase)
	if err != nil {
		return down(err, time.Since(start)), nil
	}
	defer func() { _ = db.Close() }()

	// The liveness probe proper: a trivial query that still exercises the full round trip, rather
	// than a ping the pool could answer from a connection it has not used in an hour.
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return down(classifyConnError(err), time.Since(start)), nil
	}
	latency := time.Since(start)

	status := &fwv1.HealthStatus{
		State:   fwv1.HealthState_HEALTH_STATE_UP,
		Message: "accepting connections",
		Latency: durationpb.New(latency),
	}
	status.EngineVersion = engineVersion(ctx, db)

	// Signals are collected best-effort. A monitoring account may lack VIEW SERVER STATE, and a
	// missing signal must never downgrade a healthy instance — that would turn a permissions
	// problem into a false outage.
	status.Signals = collectSignals(ctx, db)

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
func collectSignals(ctx context.Context, db *sql.DB) []*fwv1.HealthSignal {
	var signals []*fwv1.HealthSignal

	if s := connectionSignal(ctx, db); s != nil {
		signals = append(signals, s)
	}
	if s := updateabilitySignal(ctx, db); s != nil {
		signals = append(signals, s)
	}
	if s := uptimeSignal(ctx, db); s != nil {
		signals = append(signals, s)
	}
	return signals
}

// connectionSignal reports how close the instance is to its user-connection ceiling.
//
// A limit of zero means "unlimited", which SQL Server treats as 32,767. Reporting a percentage of a
// bound nobody set would be noise, so that case reports the count without a severity.
func connectionSignal(ctx context.Context, db *sql.DB) *fwv1.HealthSignal {
	var used, limit int64
	err := db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM sys.dm_exec_sessions WHERE is_user_process = 1),
		       CAST((SELECT value_in_use FROM sys.configurations WHERE name = 'user connections') AS bigint)`).
		Scan(&used, &limit)
	if err != nil {
		return nil
	}
	if limit <= 0 {
		limit = 32767
	}

	usage := float64(used) / float64(limit)
	severity := fwv1.Severity_SEVERITY_INFO
	message := "connection usage is normal"
	if usage >= connectionUsageWarn {
		severity = fwv1.Severity_SEVERITY_WARNING
		message = "connection usage is close to the configured limit"
	}

	return &fwv1.HealthSignal{
		Name:     "db.client.connection.usage",
		Severity: severity,
		Message:  message,
		Value:    usage,
		Unit:     "1",
	}
}

// updateabilitySignal reports a database that is not writable.
//
// Read-only is not unhealthy — a readable secondary is doing its job — but it changes what an
// operator may do to the instance, so it is surfaced rather than hidden.
func updateabilitySignal(ctx context.Context, db *sql.DB) *fwv1.HealthSignal {
	var updateability string
	err := db.QueryRowContext(ctx,
		`SELECT CAST(DATABASEPROPERTYEX(DB_NAME(), 'Updateability') AS nvarchar(64))`).Scan(&updateability)
	if err != nil || strings.EqualFold(updateability, "READ_WRITE") {
		return nil
	}
	return &fwv1.HealthSignal{
		Name:     "db.instance.updateability",
		Severity: fwv1.Severity_SEVERITY_INFO,
		Message:  "the instance is " + strings.ToLower(updateability),
	}
}

// uptimeSignal reports how long the instance has been up.
//
// It is informational, and it is the signal that explains an estate-wide anomaly faster than any
// other: a server that restarted an hour ago explains a missed backup window on its own.
func uptimeSignal(ctx context.Context, db *sql.DB) *fwv1.HealthSignal {
	var startedAt time.Time
	if err := db.QueryRowContext(ctx,
		"SELECT sqlserver_start_time FROM sys.dm_os_sys_info").Scan(&startedAt); err != nil {
		return nil
	}
	uptime := time.Since(startedAt)
	return &fwv1.HealthSignal{
		Name:     "db.instance.uptime",
		Severity: fwv1.Severity_SEVERITY_INFO,
		Message:  "up for " + uptime.Truncate(time.Second).String(),
		Value:    uptime.Seconds(),
		Unit:     "s",
	}
}
