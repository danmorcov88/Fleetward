// Package audit writes the record of what everyone did.
//
// The table is append-only by trigger — `audit_log_no_update` raises on UPDATE and on DELETE, since
// migration 000001 — so everything here is an INSERT and there is no correction path. That is the
// property that makes the table evidence rather than a log, and it is also why the shape of what
// goes in matters more than usual: a row cannot be fixed afterwards, and a row that should never
// have existed cannot be removed.
//
// The one rule that is enforced by construction rather than by care: **there is no function in this
// package that takes a request message.** `details` is assembled from named strings the caller
// chose, one at a time. A convenient `Record(ctx, action, req)` that marshalled the request "for
// context" would write a production database password — `CreateInstanceRequest` carries one — into
// a table that by design cannot be edited or deleted. There is no way back from that, so the
// function does not exist and this comment is why.
package audit

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// RequestInfo is what the HTTP layer knows and the audit row wants: where the request came from and
// what it said it was. Attached to the context by the server's middleware.
type RequestInfo struct {
	SourceIP  string
	UserAgent string
}

type contextKey struct{}

var requestInfoKey contextKey

// WithRequestInfo returns a context carrying the request's origin.
func WithRequestInfo(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey, info)
}

// RequestInfoFrom reads it back. A context without one — the scheduler's, for instance — yields the
// zero value, and both columns are nullable or defaulted for exactly that case.
func RequestInfoFrom(ctx context.Context) RequestInfo {
	info, _ := ctx.Value(requestInfoKey).(RequestInfo)
	return info
}

// Entry is one record. Every field is chosen by the caller; nothing is derived from a request
// message.
type Entry struct {
	// Action is what was attempted, in the vocabulary of the policy table: "backup.run",
	// "instance.create", "token.revoke".
	Action       string
	ResourceType string
	ResourceID   string
	// Succeeded is false for a refusal and for an action that was permitted and then failed. Both
	// are worth recording and the audit log does not distinguish them; the error is in the log.
	Succeeded bool
	// Details holds an allow-listed handful of strings: which fields changed, what role was
	// required, what role the caller held. Never a value a user supplied, and never a credential.
	Details map[string]string
}

// Writer records entries.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewWriter builds one.
func NewWriter(pool *pgxpool.Pool, log *slog.Logger) *Writer {
	return &Writer{pool: pool, log: log.With(slog.String("component", "audit"))}
}

// Record writes one row for the caller on ctx.
//
// A failure to write is logged and swallowed. That is a real decision with a real cost: an audit
// record can be lost while the action it describes succeeds. The alternative — failing the request
// because the audit insert failed — means a metadata hiccup stops backups running, and on a product
// whose entire purpose is that backups keep running, that trade is the wrong way round. The insert
// shares the request's connection pool and fails only when the metadata store is already failing,
// which is a condition readiness reports loudly on its own.
func (w *Writer) Record(ctx context.Context, entry Entry) {
	p, ok := authn.From(ctx)
	if !ok {
		// Nothing can be attributed, so nothing is written. Reaching here is a bug rather than a
		// client mistake, and the log line is what surfaces it.
		w.log.ErrorContext(ctx, "cannot audit an action with no principal on the context",
			slog.String("action", entry.Action))
		return
	}
	if p.TenantID == "" {
		w.log.ErrorContext(ctx, "cannot audit an action whose principal carries no tenant",
			slog.String("action", entry.Action), slog.String("actor", p.Actor))
		return
	}

	info := RequestInfoFrom(ctx)
	details := entry.Details
	if details == nil {
		details = map[string]string{}
	}

	// user_id is NULL for a system or bootstrap caller, and `actor` carries the name instead. The
	// schema separated the two from the start so that a record survives the user being deleted; it
	// also happens to be exactly what a caller with no user row needs (ADR-0036).
	var userID *string
	if p.UserID != "" {
		userID = &p.UserID
	}
	var sourceIP *string
	if info.SourceIP != "" {
		sourceIP = &info.SourceIP
	}

	if _, err := w.pool.Exec(ctx, `
		INSERT INTO audit_log (tenant_id, user_id, actor, action, resource_type, resource_id,
		                       details, source_ip, user_agent, request_id, succeeded)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		p.TenantID, userID, p.Actor, entry.Action, entry.ResourceType, entry.ResourceID,
		details, sourceIP, info.UserAgent, telemetry.RequestIDFrom(ctx), entry.Succeeded,
	); err != nil {
		w.log.ErrorContext(ctx, "failed to write an audit record",
			slog.String("action", entry.Action),
			slog.String("actor", p.Actor),
			slog.Bool("succeeded", entry.Succeeded),
			slog.String("error", err.Error()))
	}
}
