package authz

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/danmorcov88/fleetward/internal/controlplane/audit"
)

// Recorder is the slice of the audit writer this package needs.
//
// An interface rather than *audit.Writer so the coverage test can assert what a refusal records —
// which is half of what makes §7.5 provable — without a database behind it.
type Recorder interface {
	Record(ctx context.Context, entry audit.Entry)
}

// Enforcer is what the decorators share: the guard that decides, and the recorder that remembers.
type Enforcer struct {
	guard *Guard
	audit Recorder
	log   *slog.Logger
}

// NewEnforcer builds one.
func NewEnforcer(guard *Guard, recorder Recorder, log *slog.Logger) *Enforcer {
	return &Enforcer{guard: guard, audit: recorder, log: log.With(slog.String("component", "authz"))}
}

// guarded is the whole enforcement path, and every decorated method is one call to it.
//
// It is a package-level generic function rather than a method because Go does not allow type
// parameters on methods — which turns out to be a small blessing: there is exactly one copy of this
// logic and 24 one-line call sites, so a reviewer checking that every RPC is guarded is reading a
// list rather than an implementation.
//
// What is audited, and why (ADR-0035):
//
//   - A refusal of an authenticated caller is always recorded, whatever the method. Somebody who
//     *is* somebody reached for something they may not have, and that is the most interesting row
//     in an audit log. It is bounded by the number of issued credentials.
//   - An unauthenticated request is never recorded. It names no principal, so the row would carry
//     nothing an investigation could use, and it is exactly the row an attacker can generate a
//     million of. It goes to the access log instead.
//   - A permitted call is recorded when the method mutates something, or when it is one of the
//     reads worth recording. Both outcomes: an action that was allowed and then failed is a row
//     with succeeded = false, the same as a refusal, because the audit log records attempts.
func guarded[Req proto.Message, Resp proto.Message](
	e *Enforcer,
	ctx context.Context,
	method string,
	req Req,
	call func(context.Context, Req) (Resp, error),
) (Resp, error) {
	var zero Resp

	decision, err := e.guard.Check(ctx, method, req)
	if err != nil {
		if status.Code(err) != codes.Unauthenticated {
			e.recordRefusal(ctx, decision)
		}
		return zero, err
	}

	resp, callErr := call(ctx, req)

	if decision.Rule.Mutating || decision.Rule.AuditRead {
		e.recordOutcome(ctx, decision, resp, callErr)
	}
	return resp, callErr
}

func (e *Enforcer) recordRefusal(ctx context.Context, d Decision) {
	details := map[string]string{
		"method":         d.Method,
		"required_role":  d.Rule.MinRole,
		"effective_role": d.EffectiveRole,
		"outcome":        "refused",
	}
	if d.Reason != "" {
		details["reason"] = d.Reason
	}
	addScope(details, d.Scope)

	e.log.WarnContext(ctx, "refused an authorized-only action",
		slog.String("method", d.Method),
		slog.String("required_role", d.Rule.MinRole),
		slog.String("effective_role", d.EffectiveRole),
		slog.String("reason", d.Reason))

	e.audit.Record(ctx, audit.Entry{
		Action:       d.Rule.Action,
		ResourceType: d.Rule.ResourceType,
		ResourceID:   d.ResourceID,
		Succeeded:    false,
		Details:      details,
	})
}

func (e *Enforcer) recordOutcome(ctx context.Context, d Decision, resp proto.Message, callErr error) {
	details := map[string]string{
		"method":         d.Method,
		"required_role":  d.Rule.MinRole,
		"effective_role": d.EffectiveRole,
	}
	addScope(details, d.Scope)
	if callErr != nil {
		// The status code, not the message. A message can carry whatever the service put in it,
		// and this table cannot be edited afterwards.
		details["outcome"] = "failed"
		details["code"] = status.Code(callErr).String()
	} else {
		details["outcome"] = "succeeded"
	}

	resourceID := d.ResourceID
	if callErr == nil {
		if created := responseResourceID(resp); created != "" {
			if resourceID == "" {
				// A create names nothing on the way in — the resource does not exist yet — so the
				// response is where its identity first appears.
				resourceID = created
			} else {
				// Everything else acted on something that already existed, and *also* produced
				// something. `backup.run` acts on an instance and produces a backup. Overwriting
				// resource_id with the new thing would make the row say `instance` beside a
				// backup's id, which is what the B6 walk found; the new id belongs here instead.
				details["created"] = created
			}
		}
	}

	e.audit.Record(ctx, audit.Entry{
		Action:       d.Rule.Action,
		ResourceType: d.Rule.ResourceType,
		ResourceID:   resourceID,
		Succeeded:    callErr == nil,
		Details:      details,
	})
}

// addScope records which grant answered the request, which is not the same thing as what the
// request was about.
//
// The key is `authorized_by` rather than `scope`, and the distinction is the point. A tenant-wide
// grant is checked first and returns before any scope is resolved, so a `dba` with one running a
// backup on a named instance leaves an empty Scope here — and a key called `scope` reading "tenant"
// on a request that plainly named one instance is a misleading row in a table that cannot be
// corrected. What the request acted on is in `resource_id`; this says what let it through.
func addScope(details map[string]string, s Scope) {
	switch {
	case s.InstanceID != "":
		details["authorized_by"] = "a grant covering instance " + s.InstanceID
	case s.EnvironmentID != "":
		details["authorized_by"] = "a grant covering environment " + s.EnvironmentID
	default:
		details["authorized_by"] = "a tenant-wide grant"
	}
}

// responseResourceID finds the identity of something a call created.
//
// It reads the response's own fields rather than being told per method: a top-level string field
// called `id` or ending in `_id` first, then the `id` of the first message the response wraps. That
// covers CreateInstance, CreateEnvironment, CreateSchedule, CreateToken, RunBackup and
// RunVerification without a table mapping each one to where its new id lives.
func responseResourceID(msg proto.Message) string {
	if msg == nil || !msg.ProtoReflect().IsValid() {
		return ""
	}
	m := msg.ProtoReflect()
	fields := m.Descriptor().Fields()

	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() {
			continue
		}
		if fd.Kind() == protoreflect.StringKind &&
			(fd.Name() == "id" || len(fd.Name()) > 3 && fd.Name()[len(fd.Name())-3:] == "_id") {
			if v := m.Get(fd).String(); v != "" {
				return v
			}
		}
	}

	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.IsList() || fd.IsMap() || fd.Kind() != protoreflect.MessageKind || !m.Has(fd) {
			continue
		}
		nested := m.Get(fd).Message()
		idField := nested.Descriptor().Fields().ByName("id")
		if idField != nil && idField.Kind() == protoreflect.StringKind {
			if v := nested.Get(idField).String(); v != "" {
				return v
			}
		}
	}
	return ""
}
