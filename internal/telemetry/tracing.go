package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Spans, and the one place otel.Tracer is called.
//
// Four operations carry a span: an API request, a backup run, a verification, and the alert
// evaluation pass. That list is a decision rather than a starting point — each additional one is a
// name and an attribute set to get wrong, and the four are the operations an operator asks about.
//
// A span attribute may carry what a metric label may not. A span *is* one event, so `backup_id` on
// a backup span is the point rather than a cardinality problem (ADR-0041). The credential rule is
// the same on both and has no exception: nothing that could hold a password, a connection string,
// or an artifact's contents goes into an attribute, because a span leaves the process just as a
// metric does.
//
// With telemetry disabled the global provider is OTel's no-op, Start returns a non-recording span,
// and every call below costs an interface dispatch. No call site needs a guard.

// Span names, so that a rename is one edit and a dashboard built on them does not silently empty.
const (
	SpanHTTPRequest     = "fleetward.http.request"
	SpanBackupRun       = "fleetward.backup.run"
	SpanVerification    = "fleetward.verification.run"
	SpanAlertEvaluation = "fleetward.alert.evaluation"
)

// Span attribute keys that are not also metric labels.
//
// These are the per-event identifiers a metric may never carry. They are declared here rather than
// beside the labels in metrics.go precisely so that the two lists cannot be confused for one.
const (
	AttrBackupID       = "fleetward.backup.id"
	AttrVerificationID = "fleetward.verification.id"
	AttrJobID          = "fleetward.job.id"
	AttrBackupBytes    = "fleetward.backup.bytes"
	AttrBackupParts    = "fleetward.backup.parts"
	AttrRulesEvaluated = "fleetward.alert.rules_evaluated"
	AttrAlertsOpened   = "fleetward.alert.opened"
	AttrAlertsResolved = "fleetward.alert.resolved"
)

// Tracer returns Fleetward's tracer. The only call to otel.Tracer in the tree.
func Tracer() trace.Tracer { return otel.Tracer(scopeName) }

// StartSpan begins a span and returns the context carrying it.
//
// The caller always defers End on the returned span. There is no variant that does not, because a
// span that is started and never ended is a leak the exporter cannot report.
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// EndSpan records an outcome and ends the span.
//
// A nil error is Ok. A non-nil error is recorded and the status set to Error — and the message that
// reaches the span is the error's own, which is the same text the log line already carries. Nothing
// here formats a request, a connection, or a credential into it.
func EndSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// Annotate adds attributes to whatever span is on the context, or does nothing if there is none.
//
// This is how the authorization decorator puts the RPC method and the decision onto the span the
// HTTP middleware started, rather than opening a second span that somebody would then have to join
// to the first. The gateway runs in-process (ADR-0019), so the request context reaches the
// decorator unbroken.
func Annotate(ctx context.Context, attrs ...attribute.KeyValue) {
	trace.SpanFromContext(ctx).SetAttributes(attrs...)
}
