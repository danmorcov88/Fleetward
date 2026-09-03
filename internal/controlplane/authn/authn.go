// Package authn answers one question: who is making this request.
//
// It answers it and stops. What that answer is *allowed to do* belongs to the authz package, and
// keeping the two apart is what makes B10 a swap rather than a rewrite: OIDC replaces an
// implementation of Authenticator and changes nothing about roles, grants, scope or auditing
// (ADR-0033).
//
// Three kinds of caller exist, and the differences between them are load-bearing:
//
//   - A user, authenticated by an API token or by the session cookie minted from one. It has a row
//     in `users`, so audit records can point at it and `triggered_by` can be filled in.
//   - A system caller — the scheduler, the retention sweep, the job reaper. It has no row and, more
//     importantly, no credential: it is constructed in-process and nothing that parses a request
//     can produce one, so it can never be presented at the HTTP surface (ADR-0036).
//   - The bootstrap caller, which exists only while the operator has configured a bootstrap token.
//     It is never a database row, so removing the setting removes the access and leaves nothing
//     behind to find later (ADR-0033).
package authn

import (
	"context"
	"errors"
	"net/http"
)

// Sentinel errors. The API layer maps these onto status codes; nothing below decides what a client
// sees.
var (
	// ErrNoCredential reports that the request carried nothing to authenticate with.
	ErrNoCredential = errors.New("no credential presented")
	// ErrInvalidCredential reports a credential that was presented and rejected: unknown, expired,
	// revoked, or malformed. The distinction between those is deliberately not exposed — telling an
	// attacker which half of a guess was right is a favour.
	ErrInvalidCredential = errors.New("credential is not valid")
	// ErrNoPrincipal reports a context that reached code needing a caller without carrying one.
	// This is a programming error rather than a client one: every HTTP path attaches a principal,
	// and the scheduler attaches a system one.
	ErrNoPrincipal = errors.New("no principal on this context")
)

// Kind separates the sorts of caller. See the package comment for why the distinctions matter.
type Kind int

const (
	// KindAnonymous is a request that presented no credential. It holds no grants and can do
	// nothing; it exists so that "not authenticated" is a value rather than a nil pointer.
	KindAnonymous Kind = iota
	// KindUser is a person or a script, backed by a row in `users`.
	KindUser
	// KindSystem is the control plane acting on nobody's behalf. Never authenticable.
	KindSystem
	// KindBootstrap is the configured break-glass credential, or the whole world when
	// authentication is switched off in development.
	KindBootstrap
)

// String renders a kind for logging.
func (k Kind) String() string {
	switch k {
	case KindUser:
		return "user"
	case KindSystem:
		return "system"
	case KindBootstrap:
		return "bootstrap"
	case KindAnonymous:
		return "anonymous"
	default:
		return "unknown"
	}
}

// Grant is one row of `role_grants` as the guard needs it: a role, its rank, and the scope it
// covers. Exactly one of EnvironmentID and InstanceID is set, or neither, which means the grant
// covers the whole tenant — the schema enforces that with role_grants_single_scope.
type Grant struct {
	Role          string
	Rank          int
	EnvironmentID string
	InstanceID    string
}

// TenantWide reports whether this grant covers everything in the tenant.
func (g Grant) TenantWide() bool { return g.EnvironmentID == "" && g.InstanceID == "" }

// Principal is who is calling, together with everything needed to decide what they may do and to
// record what they did. It is resolved once per request and carried on the context.
type Principal struct {
	Kind Kind
	// UserID is empty for system and bootstrap principals, which have no row in `users`. It is what
	// audit_log.user_id and every `triggered_by` column are filled from.
	UserID string
	// Actor is the name that lands in audit_log.actor, which is text beside user_id precisely so
	// the record survives the user being deleted. It is never a credential.
	Actor       string
	DisplayName string
	Email       string
	TenantID    string
	// Grants is the whole grant set, resolved at authentication time so that authorizing a request
	// costs no query. See ADR-0034 for why the maximum rank wins rather than the most specific.
	Grants []Grant
	// TokenID identifies the credential that authenticated this principal, for the audit trail and
	// for last-used bookkeeping. Empty for system and bootstrap principals.
	TokenID string
}

// Authenticated reports whether anybody at all is behind this request.
func (p Principal) Authenticated() bool { return p.Kind != KindAnonymous }

// Anonymous is the principal of a request that presented no credential. It carries no tenant, which
// is what makes an unauthenticated path fail before it can read anything rather than after.
func Anonymous() Principal {
	return Principal{Kind: KindAnonymous, Actor: "anonymous"}
}

// System builds the principal for the control plane's own automatic work — "scheduler",
// "retention", "reaper". It is constructed in-process and never parsed from a request, which is the
// property that stops it being impersonable (ADR-0036).
//
// It holds no grants, and does not need any: the guard allows a system caller outright rather than
// resolving a role for it. Nothing routes one through the guard today — the scheduler calls the
// services directly — but a system caller that somehow reached one should be allowed rather than
// mysteriously refused, and giving it a synthetic `admin` row would have meant inventing a rank in
// Go that the `roles` table is the authority on.
func System(name, tenantID string) Principal {
	return Principal{
		Kind:        KindSystem,
		Actor:       "system:" + name,
		DisplayName: "Fleetward " + name,
		TenantID:    tenantID,
	}
}

// -----------------------------------------------------------------------------------------------
// The context
// -----------------------------------------------------------------------------------------------

type contextKey struct{}

var principalKey contextKey

// WithPrincipal returns a context carrying the caller. Every HTTP request gets one in the server's
// middleware; the scheduler and the retention sweep build their own with System.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// From returns the principal on the context.
func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// MustFrom returns the principal on the context or an error. Used where the absence is a bug rather
// than a client mistake.
func MustFrom(ctx context.Context) (Principal, error) {
	p, ok := From(ctx)
	if !ok {
		return Principal{}, ErrNoPrincipal
	}
	return p, nil
}

// Tenant returns the tenant of the principal on the context, or the empty string when there is
// none.
//
// The empty string is deliberate and it is a trap set on purpose. Every service query filters on
// `tenant_id = $1` against a UUID column, so an empty tenant makes Postgres reject the statement
// outright rather than quietly matching nothing — or, far worse, quietly matching the default
// tenant the way a hardcoded constant did before this slice. A code path that reaches the database
// without a principal fails on its first query, loudly, with a stack trace pointing at itself.
func Tenant(ctx context.Context) string {
	p, ok := From(ctx)
	if !ok {
		return ""
	}
	return p.TenantID
}

// -----------------------------------------------------------------------------------------------
// The seam
// -----------------------------------------------------------------------------------------------

// Authenticator turns a request into a caller. This is the whole seam between B6 and B10: OIDC
// arrives as another implementation of this interface plus one branch in the session handler, and
// everything downstream — grants, scope, the guard, the audit log — is untouched (ADR-0033).
//
// An implementation returns ErrNoCredential when the request carried nothing it recognises, so that
// a Chain can try the next one.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// Chain tries each authenticator in order and returns the first principal anybody recognises.
//
// Order matters and is fixed at construction: session cookie, then bearer token, then the bootstrap
// token. A browser presents a cookie, a script presents a token, and the operator presents the
// break-glass credential; nothing presents two, so the ordering is about cost rather than
// precedence.
type Chain []Authenticator

// Authenticate implements Authenticator.
//
// A credential that is presented and rejected stops the chain. Falling through to the next
// authenticator after an invalid token would let a wrong bearer token be silently upgraded to
// whatever the bootstrap token grants, which is the opposite of what a chain is for.
func (c Chain) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(ctx, r)
		switch {
		case err == nil:
			return p, nil
		case errors.Is(err, ErrNoCredential):
			continue
		default:
			return Principal{}, err
		}
	}
	return Principal{}, ErrNoCredential
}
