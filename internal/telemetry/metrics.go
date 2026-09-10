package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// This file is the whole answer to "what does Fleetward measure about itself". Every metric name,
// every label key, and every function that records one is here, for the same reason
// internal/storage/tsdb keeps its label names in one block: a name that is written in two places
// is a name that will eventually disagree with itself.
//
// Three rules hold throughout, and all three are ADR-0041.
//
//   - **Naming.** Where OpenTelemetry semantic conventions already name the thing being measured,
//     the semconv name is used unchanged — an operator's existing dashboards for
//     `http.server.request.duration` should work. Where they do not, because the thing is a
//     Fleetward concept, the name lives under `fleetward.`. Metrics about a *monitored database*
//     are `db.client.*` (ADR-0011), are not Fleetward's own, and never appear here.
//
//   - **Cardinality.** A label's value set must be bounded by the size of the estate. A value set
//     that grows with time is forbidden: `backup_id`, `verification_id`, `job_id`, `alert_id`,
//     `fingerprint`, `object_key` and every error message are per-event, and a histogram keyed on
//     one of them turns the metrics store into the problem it was installed to solve. Those belong
//     on a span, which is one event by construction, or in a log line.
//
//   - **No function here takes a request message, a connection, or an error.** This is the rule
//     internal/controlplane/audit states for the audit log, enforced the same way and for a sharper
//     reason. `runRequest.connection` is an *inventory.Connection and its fourth field is
//     Credentials; a convenient RecordBackup(ctx, req, err) is one %v away from putting a
//     production database password in a label, on an exporter that ships to somebody else's
//     collector — and unlike an audit row, a metric is re-emitted on every scrape forever. So every
//     recorder below takes named scalars, and an error becomes an outcome word at the call site.
//
// Everything here is safe to call with no meter provider installed, which is the state of a
// control plane running with both telemetry flags off. The global provider is then OTel's no-op and
// each recorder is a nil check and a return; no call site needs an `if enabled` guard.

// Instrument names.
const (
	// MetricHTTPServerRequestDuration is the OTel semantic convention name, deliberately.
	MetricHTTPServerRequestDuration = "http.server.request.duration"

	MetricAuthzDecisions          = "fleetward.authz.decisions"
	MetricBackupDuration          = "fleetward.backup.duration"
	MetricVerificationDuration    = "fleetward.verification.duration"
	MetricAlertEvaluationDuration = "fleetward.alert.evaluation.duration"
	MetricAlertsOpened            = "fleetward.alerts.opened"
	MetricAlertsResolved          = "fleetward.alerts.resolved"
	MetricNotifications           = "fleetward.notifications"
)

// Label keys. Every one is bounded by the estate or by a closed set in the schema or the contract;
// see the cardinality rule above, and the test that refuses a new one that is not.
const (
	AttrHTTPRequestMethod = "http.request.method"
	AttrHTTPRoute         = "http.route"
	AttrHTTPStatusCode    = "http.response.status_code"

	// AttrRPCMethod is bounded by authz.Policies, which the coverage test keeps closed.
	AttrRPCMethod          = "fleetward.rpc.method"
	AttrAuthzOutcome       = "fleetward.authz.outcome"
	AttrEffectiveRole      = "fleetward.authz.effective_role"
	AttrInstanceID         = "fleetward.instance.id"
	AttrEngineType         = "fleetward.engine.type"
	AttrBackupMethod       = "fleetward.backup.method"
	AttrVerificationStatus = "fleetward.verification.status"
	AttrAlertKind          = "fleetward.alert.kind"
	AttrAlertSeverity      = "fleetward.alert.severity"
	AttrNotifierKind       = "fleetward.notifier.kind"
	AttrOutcome            = "fleetward.outcome"
)

// Outcome words. Small closed vocabularies rather than free text, which is what keeps the
// cardinality rule true by construction rather than by care.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeCompleted = "completed"

	OutcomeAllowed = "allowed"
	OutcomeRefused = "refused"

	OutcomeDelivered = "delivered"
	// OutcomeDropped is a notification the bounded queue refused. It is the counter that makes
	// ADR-0039's at-most-once delivery visible rather than merely documented.
	OutcomeDropped = "dropped"
)

// Bucket boundaries, per instrument.
//
// These exist because OTel's defaults stop at 10 seconds. A backup takes minutes to hours and a
// verification pulls a container image before it starts, so with the defaults every one of them
// lands in the +Inf bucket and the histogram answers nothing — while looking, from the outside,
// exactly like a working instrument.
//
// They are attached as instrument *advisory* boundaries rather than as SDK views. The SDK honours
// an advisory when no view overrides it, and this keeps every fact about a metric — its name, its
// unit, its labels and its resolution — in this one file, which is the point of the file.
var (
	httpDurationBuckets = []float64{
		0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10,
	}
	// Out to four hours. A logical dump of a large database is an hours-long operation and the
	// question an operator asks is "is it slower than it used to be", which needs resolution
	// where the values actually are.
	backupDurationBuckets = []float64{
		1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400,
	}
	// Out to an hour. A verification is a restore into a fresh container, so its floor is an image
	// pull rather than a network round trip.
	verificationDurationBuckets = []float64{
		5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600,
	}
	// The pass runs every thirty seconds by default, so anything past a minute is already a
	// problem and the top buckets exist to show it.
	alertEvaluationBuckets = []float64{
		0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
	}
)

// scopeName identifies Fleetward as the instrumentation library, which is what the exporter renders
// as `otel_scope_name` and what distinguishes these series from anything else in the same store.
const scopeName = "github.com/danmorcov88/fleetward"

// instruments holds every instrument, created once.
type instruments struct {
	httpDuration           metric.Float64Histogram
	authzDecisions         metric.Int64Counter
	backupDuration         metric.Float64Histogram
	verificationDuration   metric.Float64Histogram
	alertEvaluationSeconds metric.Float64Histogram
	alertsOpened           metric.Int64Counter
	alertsResolved         metric.Int64Counter
	notifications          metric.Int64Counter
}

// defaultInstruments builds them lazily, on the first recorded measurement.
//
// Lazily because Setup installs the meter provider during startup and the first measurement comes
// after it; and once because an instrument is cheap to hold and not free to create. Instruments
// created before a provider is installed keep working: the global meter delegates, which is the
// property that lets this be a package-level detail rather than a wiring problem at every call site.
var defaultInstruments = sync.OnceValue(func() *instruments { return newInstruments(otel.Meter(scopeName)) })

// active returns the instruments the recorders write to.
//
// A variable rather than a direct call, and the only indirection in this file. It exists so the
// cardinality test can point the recorders at an SDK pipeline it can read back, and therefore check
// ADR-0041's rule against the attribute keys that are actually *emitted* rather than against a list
// somebody has to remember to update. A list would not catch the mistake the rule is about.
var active = func() *instruments { return defaultInstruments() }

func newInstruments(m metric.Meter) *instruments {
	return &instruments{
		httpDuration: histogram(m, MetricHTTPServerRequestDuration, "s",
			"Duration of HTTP server requests.", httpDurationBuckets),
		authzDecisions: counter(m, MetricAuthzDecisions, "{decision}",
			"Authorization decisions, by RPC method and outcome."),
		backupDuration: histogram(m, MetricBackupDuration, "s",
			"Duration of a backup run, from the first plugin call to the recorded outcome.",
			backupDurationBuckets),
		verificationDuration: histogram(m, MetricVerificationDuration, "s",
			"Duration of a verification, including provisioning and tearing down the sandbox.",
			verificationDurationBuckets),
		alertEvaluationSeconds: histogram(m, MetricAlertEvaluationDuration, "s",
			"Duration of one alert evaluation pass over the estate.", alertEvaluationBuckets),
		alertsOpened: counter(m, MetricAlertsOpened, "{alert}",
			"Alerts opened by an evaluation pass, by rule kind and severity."),
		alertsResolved: counter(m, MetricAlertsResolved, "{alert}",
			"Alerts resolved by an evaluation pass, by rule kind."),
		notifications: counter(m, MetricNotifications, "{notification}",
			"Alert notifications, by destination kind and outcome."),
	}
}

// histogram creates one, and never returns nil.
//
// A nil metric.Float64Histogram panics on Record, and it would panic on a path that only runs in
// production — the first backup, not any unit test. So a creation failure yields a working no-op
// instrument and a log line, and the control plane keeps taking backups.
func histogram(m metric.Meter, name, unit, description string, boundaries []float64) metric.Float64Histogram {
	h, err := m.Float64Histogram(name,
		metric.WithUnit(unit),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(boundaries...))
	if err != nil || h == nil {
		slog.Warn("could not create a metric instrument; it will record nothing",
			slog.String("metric", name),
			slog.String("error", errText(err)))
		h, _ = noop.NewMeterProvider().Meter(scopeName).Float64Histogram(name)
	}
	return h
}

func counter(m metric.Meter, name, unit, description string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithUnit(unit), metric.WithDescription(description))
	if err != nil || c == nil {
		slog.Warn("could not create a metric instrument; it will record nothing",
			slog.String("metric", name),
			slog.String("error", errText(err)))
		c, _ = noop.NewMeterProvider().Meter(scopeName).Int64Counter(name)
	}
	return c
}

func errText(err error) string {
	if err == nil {
		return "the meter returned no instrument"
	}
	return err.Error()
}

// -----------------------------------------------------------------------------------------------
// Recording
// -----------------------------------------------------------------------------------------------
//
// Every function below takes named scalars and returns nothing. The context is used only to attach
// an exemplar's trace context; a cancelled context does not prevent a measurement being recorded,
// which is what makes it correct to record an outcome on the way out of a cancelled run.

// RecordHTTPRequest records one served request.
//
// The route must be a *pattern* and never a path. `/api/v1/backups/<uuid>/verifications` as a label
// value is one series per backup, forever.
func RecordHTTPRequest(ctx context.Context, method, route string, status int, d time.Duration) {
	active().httpDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrHTTPRequestMethod, method),
		attribute.String(AttrHTTPRoute, route),
		attribute.Int(AttrHTTPStatusCode, status),
	))
}

// RecordAuthzDecision records one authorization outcome.
//
// This is where a 403 becomes countable. `effectiveRole` is the role the caller actually held, which
// is what makes "who is being refused, and what do they hold" answerable without reading the audit
// log — and it is empty for a caller holding nothing, which is a value rather than a gap.
func RecordAuthzDecision(ctx context.Context, rpcMethod, outcome, effectiveRole string) {
	active().authzDecisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrRPCMethod, rpcMethod),
		attribute.String(AttrAuthzOutcome, outcome),
		attribute.String(AttrEffectiveRole, effectiveRole),
	))
}

// RecordBackup records one finished backup run, managed or manual.
//
// engineType is a label value and not a branch: nothing in this package or its callers decides what
// to record by reading it, which is the capability-driven rule of CLAUDE.md §4.1 applied to
// telemetry. An engine's name as data is fine; an engine's name as a condition is not.
func RecordBackup(ctx context.Context, instanceID, engineType, backupMethod, outcome string, d time.Duration) {
	active().backupDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrInstanceID, instanceID),
		attribute.String(AttrEngineType, engineType),
		attribute.String(AttrBackupMethod, backupMethod),
		attribute.String(AttrOutcome, outcome),
	))
}

// RecordVerification records one finished verification.
//
// status carries all three verdicts, including INCONCLUSIVE, because the distinction between a
// proven-bad artifact and a sandbox that never started is the whole of ADR-0022 and collapsing it
// here would hide exactly the failure ADR-0040 refuses to hide in alerting.
func RecordVerification(ctx context.Context, instanceID, engineType, status string, d time.Duration) {
	active().verificationDuration.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrInstanceID, instanceID),
		attribute.String(AttrEngineType, engineType),
		attribute.String(AttrVerificationStatus, status),
	))
}

// RecordAlertEvaluation records one pass over the estate.
//
// Its `_count` is the answer to "did the evaluation pass run last night", which ADR-0038 left to a
// log line because a pass writes no job row. This is the metric that closes that.
func RecordAlertEvaluation(ctx context.Context, outcome string, d time.Duration) {
	active().alertEvaluationSeconds.Record(ctx, d.Seconds(), metric.WithAttributes(
		attribute.String(AttrOutcome, outcome),
	))
}

// RecordAlertOpened records one alert a pass created — and therefore one notification it sent.
func RecordAlertOpened(ctx context.Context, kind, severity string) {
	active().alertsOpened.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrAlertKind, kind),
		attribute.String(AttrAlertSeverity, severity),
	))
}

// RecordAlertResolved records one open alert whose condition disappeared.
func RecordAlertResolved(ctx context.Context, kind string) {
	active().alertsResolved.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrAlertKind, kind),
	))
}

// RecordNotification records one delivery attempt's outcome, or one that was never attempted.
//
// notifierKind is `webhook` or `smtp`. The destination itself is never a label: a webhook URL can
// embed its own credential, which is why B7's transports describe failures rather than wrapping
// errors that carry the URL, and a label would put it somewhere far more durable than a log line.
func RecordNotification(ctx context.Context, notifierKind, outcome string) {
	active().notifications.Add(ctx, 1, metric.WithAttributes(
		attribute.String(AttrNotifierKind, notifierKind),
		attribute.String(AttrOutcome, outcome),
	))
}
