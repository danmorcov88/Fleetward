package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/version"
)

// ShutdownFunc flushes and stops the telemetry pipeline. Callers should defer it and give it a
// context with a deadline: a hanging collector must not delay shutdown indefinitely.
type ShutdownFunc func(context.Context) error

// Telemetry is what Setup installed.
//
// It exists because there are now two ways metrics leave this process and only one of them is a
// push: the OTLP exporter needs nothing from the caller, while the Prometheus endpoint is a handler
// somebody has to mount on a mux. Returning it here keeps the decision about *who may scrape it*
// where it belongs — in the API package, next to every other authorization decision — rather than
// in this one.
type Telemetry struct {
	// MetricsHandler serves the Prometheus exposition format, or is nil when the endpoint is
	// disabled. A nil handler is the signal not to register the route at all, so that a disabled
	// endpoint answers 404 rather than an empty 200.
	MetricsHandler http.Handler
	// Shutdown flushes and stops everything that was installed. Never nil.
	Shutdown ShutdownFunc
}

// Setup installs the global tracer and meter providers, and builds the /metrics handler.
//
// Two independent switches, because they are two different things (ADR-0041's sibling decision,
// recorded in docs/ops/observability.md):
//
//   - cfg.Enabled turns on **push**: spans and periodic metric export to an OTLP collector. It is
//     off by default, because it names an endpoint that has to exist.
//   - cfg.PrometheusEnabled turns on **pull**: the /metrics endpoint. It is on by default, because
//     it is how a Go service is monitored and it requires nothing else to be running.
//
// A meter provider is installed when either is on, with one reader per enabled path. Tracing is
// installed only for OTLP, because a span has no pull endpoint to be scraped from.
//
// When neither is on, this installs the W3C propagators and returns a no-op shutdown — and the
// global providers stay OTel's no-ops, so instrumentation code needs no `if enabled` guards at any
// call site. That is a property the recorders in metrics.go rely on and a test asserts.
func Setup(ctx context.Context, cfg config.TelemetryConfig, log *slog.Logger) (*Telemetry, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	var shutdowns []ShutdownFunc
	// shutdownAll runs every registered shutdown even if an earlier one fails, so that one stuck
	// exporter cannot strand the others.
	shutdownAll := func(ctx context.Context) error {
		var errs []error
		for _, fn := range shutdowns {
			if err := fn(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	if !cfg.Enabled && !cfg.PrometheusEnabled {
		log.Warn("FLEETWARD IS NOT OBSERVABLE: /metrics is disabled and nothing is exported",
			slog.String("consequence", "a control plane that has stopped evaluating alerts looks "+
				"exactly like an estate with nothing wrong"),
			slog.String("remedy", "set FLEETWARD_TELEMETRY_PROMETHEUS_ENABLED=true"))
		return &Telemetry{Shutdown: shutdownAll}, nil
	}

	// NewSchemaless, not NewWithAttributes(semconv.SchemaURL, …).
	//
	// resource.Merge refuses two resources that declare *different* schema URLs, and the SDK's own
	// Default() declares whichever semconv version that release was built against — 1.43.0 in
	// v1.46.0, while this file's semconv import is 1.26.0. A schemaless resource merges with any of
	// them, so an SDK upgrade cannot turn the control plane into one that refuses to start.
	//
	// It could, and did: this code was written in the foundation slice and had never run, because
	// Setup returned before reaching it whenever telemetry was disabled — which was the default and
	// therefore always. B8 turns the metrics endpoint on by default, and the first `docker compose
	// up` after that failed with `conflicting Schema URL`. The attribute keys are the stable part
	// and are what semconv is used for here.
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(version.Version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	var readers []sdkmetric.Reader
	var metricsHandler http.Handler

	if cfg.PrometheusEnabled {
		// Fleetward's own registry rather than prometheus.DefaultRegisterer. A library that
		// registers a collector on the default registry — and several do — would otherwise appear
		// in Fleetward's scrape without anybody deciding it should.
		registry := prometheus.NewRegistry()

		// The Go runtime and the process, which is the other half of what an operator means by
		// "how do I monitor it": goroutines, heap, open file descriptors, CPU. Three lines on a
		// registry we already own, and no substitute exists the way target_info substitutes for a
		// build-info metric.
		registry.MustRegister(collectors.NewGoCollector())
		registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

		// WithoutScopeInfo drops `otel_scope_name`, `otel_scope_version` and `otel_scope_schema_url`
		// from every series. There is exactly one instrumentation scope in this binary, so all three
		// carry the same value on every sample — two of them empty — and a label that never varies
		// is storage and screen width spent on nothing. It also keeps a future scope *version* from
		// starting a fresh set of series on every release.
		exporter, err := otelprom.New(
			otelprom.WithRegisterer(registry),
			otelprom.WithoutScopeInfo(),
		)
		if err != nil {
			return &Telemetry{Shutdown: shutdownAll}, fmt.Errorf("create prometheus exporter: %w", err)
		}
		readers = append(readers, exporter)

		metricsHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{
			// A collector that fails should degrade the scrape rather than fail it: a broken
			// runtime collector must not take the alert-evaluation counter with it.
			ErrorHandling: promhttp.ContinueOnError,
			ErrorLog:      slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		})
	}

	if cfg.Enabled {
		traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		}
		traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
		if err != nil {
			return &Telemetry{Shutdown: shutdownAll}, fmt.Errorf("create otlp trace exporter: %w", err)
		}

		tracerProvider := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(traceExporter),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		)
		otel.SetTracerProvider(tracerProvider)
		shutdowns = append(shutdowns, tracerProvider.Shutdown)

		metricOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint)}
		if cfg.OTLPInsecure {
			metricOpts = append(metricOpts, otlpmetricgrpc.WithInsecure())
		}
		metricExporter, err := otlpmetricgrpc.New(ctx, metricOpts...)
		if err != nil {
			return &Telemetry{Shutdown: shutdownAll}, fmt.Errorf("create otlp metric exporter: %w", err)
		}
		readers = append(readers, sdkmetric.NewPeriodicReader(
			metricExporter,
			sdkmetric.WithInterval(30*time.Second),
		))
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	meterProvider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(meterProvider)
	shutdowns = append(shutdowns, meterProvider.Shutdown)

	log.Info("telemetry installed",
		slog.Bool("metrics_endpoint", cfg.PrometheusEnabled),
		slog.Bool("otlp_export", cfg.Enabled),
		slog.String("otlp_endpoint", otlpEndpointOrNone(cfg)),
		slog.Float64("sample_ratio", cfg.SampleRatio))

	return &Telemetry{MetricsHandler: metricsHandler, Shutdown: shutdownAll}, nil
}

// otlpEndpointOrNone keeps the startup line honest: naming an endpoint that is not being used
// invites somebody to debug a collector that was never contacted.
func otlpEndpointOrNone(cfg config.TelemetryConfig) string {
	if !cfg.Enabled {
		return "(not exporting)"
	}
	return cfg.OTLPEndpoint
}
