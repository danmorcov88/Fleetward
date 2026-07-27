// Package telemetry wires Fleetward's own observability: structured logging (ADR-0014) and
// OpenTelemetry traces and metrics (ADR-0011).
//
// This package is about Fleetward observing itself. Metrics *about monitored databases* are a
// different concern entirely — those are emitted by plugins under OpenTelemetry database semantic
// conventions and stored in VictoriaMetrics.
package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/danmorcov88/fleetward/internal/config"
)

// contextKey is unexported so no other package can collide with our context values.
type contextKey int

const (
	tenantIDKey contextKey = iota
	requestIDKey
	jobIDKey
	principalKey
)

// NewLogger builds the application logger. JSON in production for log pipelines, text in
// development for humans.
func NewLogger(cfg config.LogConfig, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:     parseLevel(cfg.Level),
		AddSource: cfg.AddSource,
	}

	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}

	// contextHandler promotes correlation identifiers out of the context so that callers do not
	// have to thread them through every log call — and, more importantly, cannot forget to.
	return slog.New(&contextHandler{Handler: handler})
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler copies correlation identifiers from the context into every record.
type contextHandler struct{ slog.Handler }

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if v, ok := ctx.Value(tenantIDKey).(string); ok && v != "" {
		r.AddAttrs(slog.String("tenant_id", v))
	}
	if v, ok := ctx.Value(requestIDKey).(string); ok && v != "" {
		r.AddAttrs(slog.String("request_id", v))
	}
	if v, ok := ctx.Value(jobIDKey).(string); ok && v != "" {
		r.AddAttrs(slog.String("job_id", v))
	}
	if v, ok := ctx.Value(principalKey).(string); ok && v != "" {
		r.AddAttrs(slog.String("principal", v))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}

// WithTenantID returns a context whose log records carry the tenant identifier.
func WithTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantIDKey, id)
}

// WithRequestID returns a context whose log records carry the request identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithJobID returns a context whose log records carry the job identifier.
func WithJobID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, jobIDKey, id)
}

// WithPrincipal returns a context whose log records identify the acting principal.
func WithPrincipal(ctx context.Context, principal string) context.Context {
	return context.WithValue(ctx, principalKey, principal)
}

// TenantIDFrom returns the tenant identifier carried by ctx, if any.
func TenantIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(tenantIDKey).(string)
	return v
}

// RequestIDFrom returns the request identifier carried by ctx, if any.
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey).(string)
	return v
}

// Redacted is a log value that renders as a fixed placeholder. Wrap anything sensitive in it so
// that a value which must be present in a struct cannot accidentally be logged.
type Redacted struct{}

// LogValue implements slog.LogValuer.
func (Redacted) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// String implements fmt.Stringer so the placeholder survives fmt formatting too.
func (Redacted) String() string { return "[redacted]" }
