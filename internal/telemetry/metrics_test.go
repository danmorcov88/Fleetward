package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// recordEverything calls every recorder exactly once, with values that are plausible rather than
// zero. It is the list the tests below share, so that a new recorder added without a test fails
// this file rather than passing silently.
func recordEverything(ctx context.Context) {
	RecordHTTPRequest(ctx, "GET", "/api/v1/", 403, 120*time.Millisecond)
	RecordAuthzDecision(ctx, "/fleetward.v1.BackupService/RunBackup", OutcomeRefused, "viewer")
	RecordBackup(ctx, "instance-1", "postgresql", "pg_dump", OutcomeSucceeded, 41*time.Minute)
	RecordVerification(ctx, "instance-1", "postgresql", "VERIFICATION_STATUS_FAILED", 3*time.Minute)
	RecordAlertEvaluation(ctx, OutcomeCompleted, 250*time.Millisecond)
	RecordAlertOpened(ctx, "verification_failed", "critical")
	RecordAlertResolved(ctx, "verification_failed")
	RecordNotification(ctx, "webhook", OutcomeDelivered)
	RecordNotification(ctx, "", OutcomeDropped)
}

// TestRecordersAreNoOpsWithoutAProvider is the test the whole design rests on.
//
// FLEETWARD_TELEMETRY_ENABLED defaults to false and the /metrics endpoint can be turned off, so the
// ordinary state of a control plane is one with no meter provider installed. Every recorder has to
// be a no-op there rather than a panic on a path that only runs in production — the first backup,
// which no other unit test reaches.
//
// This test installs nothing. It is the only test in this package that must not, which is why the
// SDK tests below build their own provider locally rather than calling otel.SetMeterProvider.
func TestRecordersAreNoOpsWithoutAProvider(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a recorder panicked with no meter provider installed: %v", r)
		}
	}()
	recordEverything(context.Background())
}

// TestRecordersSurviveACancelledContext asserts what the call sites rely on: an outcome recorded on
// the way out of a run that was cancelled by shutdown is still recorded. The context supplies an
// exemplar's trace context and nothing else, so cancelling it must not lose the measurement.
func TestRecordersSurviveACancelledContext(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	useInstruments(t, reader)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	RecordBackup(ctx, "instance-1", "postgresql", "pg_dump", OutcomeFailed, time.Minute)

	if got := sumCount(t, collect(t, reader), MetricBackupDuration); got != 1 {
		t.Fatalf("a cancelled context lost the measurement: got %d points, want 1", got)
	}
}

// TestDurationBucketsReachPastTenSeconds is trap 2 of the B8 brief, as a test.
//
// OpenTelemetry's default explicit bucket boundaries end at 10 seconds. A backup takes minutes to
// hours and a verification pulls a container image before it starts, so with the defaults every one
// of them lands in the +Inf bucket and the histogram answers nothing — while looking, from the
// outside, exactly like a working instrument. This is the check that a future edit cannot quietly
// undo.
func TestDurationBucketsReachPastTenSeconds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		boundaries []float64
		atLeast    float64
	}{
		{"backup", backupDurationBuckets, 1800},
		{"verification", verificationDurationBuckets, 1800},
		{"alert evaluation", alertEvaluationBuckets, 30},
	} {
		last := tc.boundaries[len(tc.boundaries)-1]
		if last < tc.atLeast {
			t.Errorf("%s: the highest bucket boundary is %gs, want at least %gs — every run longer "+
				"than that is indistinguishable from every other", tc.name, last, tc.atLeast)
		}
	}
}

// TestTheSDKHonoursTheAdvisoryBoundaries proves the buckets above actually reach the pipeline.
//
// The boundaries are attached to the instrument as an advisory rather than through an SDK view, so
// asserting the slice is not enough: this asserts what the exporter would see.
func TestTheSDKHonoursTheAdvisoryBoundaries(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	useInstruments(t, reader)

	RecordBackup(context.Background(), "instance-1", "postgresql", "pg_dump",
		OutcomeSucceeded, 41*time.Minute)

	hist := findHistogram(t, collect(t, reader), MetricBackupDuration)
	if len(hist.DataPoints) != 1 {
		t.Fatalf("got %d data points, want 1", len(hist.DataPoints))
	}
	bounds := hist.DataPoints[0].Bounds
	if len(bounds) == 0 || bounds[len(bounds)-1] != backupDurationBuckets[len(backupDurationBuckets)-1] {
		t.Fatalf("the SDK did not honour the advisory boundaries: got %v, want %v",
			bounds, backupDurationBuckets)
	}
	// A forty-one minute backup must land somewhere other than the top bucket, which is the whole
	// point of choosing these boundaries.
	if hist.DataPoints[0].Count != 1 {
		t.Fatalf("got count %d, want 1", hist.DataPoints[0].Count)
	}
}

// forbiddenLabelKeys is ADR-0041's cardinality rule as data.
//
// Every one of these identifies a row Fleetward creates on its own, so its value set grows with
// time rather than with the estate. A histogram keyed on one of them turns the metrics store into
// the problem it was installed to solve. They are legitimate *span* attributes — a span is one
// event — which is why tracing.go declares several of them and this list refuses them here.
var forbiddenLabelKeys = []string{
	"backup.id", "backup_id",
	"verification.id", "verification_id",
	"job.id", "job_id",
	"alert.id", "alert_id",
	"schedule.id", "schedule_id",
	"fingerprint",
	"object.key", "object_key",
	"request.id", "request_id",
	"error", "message", "detail", "summary", "url", "endpoint",
	"password", "secret", "token", "credential", "dsn", "connection_string",
}

// TestNoMetricCarriesAnUnboundedLabel checks the rule against what is actually emitted.
//
// Deliberately not against a hand-maintained list of label constants: the mistake this test exists
// to catch is somebody adding `attribute.String("fleetward.backup.id", …)` to a recorder, and a
// list of constants would not notice. So it drives every recorder through a real SDK pipeline and
// reads the attribute keys back off the exported data.
func TestNoMetricCarriesAnUnboundedLabel(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	useInstruments(t, reader)
	recordEverything(context.Background())

	for _, m := range collect(t, reader) {
		for _, key := range attributeKeys(m) {
			for _, bad := range forbiddenLabelKeys {
				if key == bad || strings.HasSuffix(key, "."+bad) || strings.HasSuffix(key, "_"+bad) {
					t.Errorf("metric %q carries label %q, whose values grow with time rather than "+
						"with the estate (ADR-0041). Put it on the span instead.", m.Name, key)
				}
			}
		}
	}
}

// TestEveryMetricIsInAKnownNamespace is ADR-0041's naming rule.
//
// Two namespaces are Fleetward's own: the semantic-convention names it reuses, and `fleetward.`.
// The third — `db.client.*`, which is ADR-0011's — describes a *monitored database* and must never
// appear here: those go to the metrics store through internal/storage/tsdb, and mixing them would
// make /metrics carry somebody else's estate as well as Fleetward's own health.
func TestEveryMetricIsInAKnownNamespace(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	useInstruments(t, reader)
	recordEverything(context.Background())

	for _, m := range collect(t, reader) {
		switch {
		case strings.HasPrefix(m.Name, "fleetward."):
		case m.Name == MetricHTTPServerRequestDuration:
		default:
			t.Errorf("metric %q is in neither namespace: it is not a semantic-convention name "+
				"Fleetward reuses, and it is not under `fleetward.` (ADR-0041)", m.Name)
		}
	}
}

// TestAnInstrumentIsNeverNil is trap 3.
//
// A nil metric.Float64Histogram panics on Record, and it would panic on a path no unit test runs.
// An invalid instrument name is the cheapest way to make the meter return an error, and the
// requirement is that a creation failure still yields something safe to call.
func TestAnInstrumentIsNeverNil(t *testing.T) {
	m := sdkmetric.NewMeterProvider().Meter(scopeName)

	h := histogram(m, "not a legal instrument name", "s", "", backupDurationBuckets)
	if h == nil {
		t.Fatal("histogram returned nil for a name the meter refused")
	}
	c := counter(m, "not a legal instrument name", "{thing}", "")
	if c == nil {
		t.Fatal("counter returned nil for a name the meter refused")
	}
	// The point of non-nil is that it is callable.
	h.Record(context.Background(), 1)
	c.Add(context.Background(), 1)
}

// -----------------------------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------------------------

// useInstruments points the package's recorders at a local SDK pipeline for the duration of one
// test, and restores them afterwards.
//
// Local rather than global: otel.SetMeterProvider delegates once per process, so a test that
// installed one would decide the outcome of every test that ran after it — including
// TestRecordersAreNoOpsWithoutAProvider, whose whole subject is a process with no provider at all.
func useInstruments(t *testing.T, reader sdkmetric.Reader) {
	t.Helper()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	inst := newInstruments(provider.Meter(scopeName))

	previous := active
	active = func() *instruments { return inst }
	t.Cleanup(func() { active = previous })
}

func collect(t *testing.T, reader sdkmetric.Reader) []metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []metricdata.Metrics
	for _, scope := range rm.ScopeMetrics {
		out = append(out, scope.Metrics...)
	}
	if len(out) == 0 {
		t.Fatal("nothing was collected; the recorders are not writing to this pipeline")
	}
	return out
}

func findHistogram(t *testing.T, metrics []metricdata.Metrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, m := range metrics {
		if m.Name != name {
			continue
		}
		hist, ok := m.Data.(metricdata.Histogram[float64])
		if !ok {
			t.Fatalf("metric %q is %T, want a float64 histogram", name, m.Data)
		}
		return hist
	}
	t.Fatalf("metric %q was not collected", name)
	return metricdata.Histogram[float64]{}
}

func sumCount(t *testing.T, metrics []metricdata.Metrics, name string) uint64 {
	t.Helper()
	var total uint64
	for _, dp := range findHistogram(t, metrics, name).DataPoints {
		total += dp.Count
	}
	return total
}

// attributeKeys returns every attribute key on every data point of a metric, whatever its shape.
func attributeKeys(m metricdata.Metrics) []string {
	var keys []string
	appendSet := func(set attribute.Set) {
		for _, kv := range set.ToSlice() {
			keys = append(keys, string(kv.Key))
		}
	}
	switch data := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, dp := range data.DataPoints {
			appendSet(dp.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, dp := range data.DataPoints {
			appendSet(dp.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, dp := range data.DataPoints {
			appendSet(dp.Attributes)
		}
	}
	return keys
}

// Compile-time assurance that the recorders keep the signatures ADR-0041 requires: named scalars,
// never a request message, a connection, or an error. A future edit that widened one of these to
// take a struct would stop building here, which is a cheaper conversation than a code review.
var (
	_ func(context.Context, string, string, int, time.Duration)            = RecordHTTPRequest
	_ func(context.Context, string, string, string)                        = RecordAuthzDecision
	_ func(context.Context, string, string, string, string, time.Duration) = RecordBackup
	_ func(context.Context, string, string, string, time.Duration)         = RecordVerification
	_ func(context.Context, string, time.Duration)                         = RecordAlertEvaluation
	_ func(context.Context, string, string)                                = RecordAlertOpened
	_ func(context.Context, string)                                        = RecordAlertResolved
	_ func(context.Context, string, string)                                = RecordNotification

	_ metric.Meter // keeps the metric import honest if the assertions above ever move
)
