package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
)

// Guard applies Policies to a caller.
type Guard struct {
	pool  *pgxpool.Pool
	ranks map[string]int
	log   *slog.Logger
}

// NewGuard builds the guard, reading the role ranks from the database.
//
// The ranks are not constants in Go. Migration 000001 seeded `roles` with viewer 10, operator 20,
// dba 30 and admin 40, and `role_grants.role_name` is ON DELETE RESTRICT against that table — the
// ranks are facts in this database, and a Go constant that disagreed with them would be a bug
// nothing surfaced until somebody edited one of the two.
func NewGuard(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*Guard, error) {
	ranks, err := LoadRanks(ctx, pool)
	if err != nil {
		return nil, err
	}
	if err := ValidatePolicies(ranks); err != nil {
		return nil, err
	}
	return &Guard{pool: pool, ranks: ranks, log: log.With(slog.String("component", "authz"))}, nil
}

// LoadRanks reads the seeded role ordering.
func LoadRanks(ctx context.Context, pool *pgxpool.Pool) (map[string]int, error) {
	rows, err := pool.Query(ctx, `SELECT name, rank FROM roles`)
	if err != nil {
		return nil, fmt.Errorf("load roles: %w", err)
	}
	defer rows.Close()

	ranks := make(map[string]int)
	for rows.Next() {
		var name string
		var rank int
		if err := rows.Scan(&name, &rank); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		ranks[name] = rank
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read roles: %w", err)
	}
	if len(ranks) == 0 {
		return nil, errors.New("the roles table is empty; migration 000001 seeds it")
	}
	return ranks, nil
}

// Ranks exposes the loaded ordering, for rendering a role beside a name.
func (g *Guard) Ranks() map[string]int { return g.ranks }

// Decision is what the guard concluded, carried to the audit writer so that a record can name what
// was attempted as well as who attempted it.
type Decision struct {
	Method        string
	Rule          Rule
	Scope         Scope
	ResourceID    string
	EffectiveRole string
	// Allowed is false on a refusal. A refusal of a mutating call is audited exactly like a success
	// of one, with succeeded = false; it is the most interesting row in the log (ADR-0035).
	Allowed bool
	// Reason explains a refusal in the server's own words. It is logged and recorded; the client is
	// told only that permission was denied.
	Reason string
}

// Check decides whether the caller on ctx may invoke method with req.
//
// It returns a Decision on both outcomes, because the caller has to audit a refusal as well as a
// success, and a gRPC status only on a refusal.
func (g *Guard) Check(ctx context.Context, method string, req proto.Message) (Decision, error) {
	d := Decision{Method: method}

	p, ok := authn.From(ctx)
	if !ok || !p.Authenticated() {
		// Deliberately not audited. An unauthenticated request names no principal, so the row would
		// carry nothing an investigation could use, and it is precisely the row an attacker can
		// generate a million of (ADR-0035).
		return d, status.Error(codes.Unauthenticated,
			"this endpoint requires a credential: send Authorization: Bearer <token>, "+
				"or exchange one for a session at POST /api/v1/sessions")
	}

	rule, known := Policies[method]
	if !known {
		// Fail closed. A method with no policy is denied to everybody, including an administrator,
		// until somebody writes down what it needs. Startup validation and the coverage test both
		// exist so this is never reached in a build that shipped.
		g.log.ErrorContext(ctx, "refusing a method with no authorization policy",
			slog.String("method", method))
		return d, status.Error(codes.PermissionDenied,
			"this endpoint has no authorization policy and is refused to everyone")
	}
	d.Rule = rule
	d.ResourceID = resourceID(rule, req)

	// System and bootstrap callers are allowed outright. A system caller is constructed in-process
	// and never parsed from a request, so it cannot be presented at the HTTP surface; the bootstrap
	// caller is the operator's break-glass credential and is tenant-wide admin by definition.
	if p.Kind == authn.KindSystem || p.Kind == authn.KindBootstrap {
		d.Allowed = true
		d.EffectiveRole = RoleAdmin
		return d, nil
	}

	if rule.AnyAuthenticated {
		d.Allowed = true
		d.EffectiveRole = highestRole(p.Grants)
		return d, nil
	}

	required, ok := g.ranks[rule.MinRole]
	if !ok {
		return d, status.Errorf(codes.Internal, "unknown role %q in policy", rule.MinRole)
	}

	// 1. A tenant-wide grant covers everything, so it is checked first and it is the only path that
	//    performs no query at all. That is the estate view's path, sixty times a minute.
	if best := bestRank(p.Grants, func(gr authn.Grant) bool { return gr.TenantWide() }); best >= required {
		d.Allowed = true
		d.EffectiveRole = roleAtLeast(p.Grants, best)
		return d, nil
	}

	scope, err := g.resolveScope(ctx, rule, req, p.TenantID)
	if err != nil {
		if errors.Is(err, ErrScopeUnresolvable) {
			d.Reason = "the resource named does not exist in this tenant"
			return d, status.Error(codes.PermissionDenied, permissionDeniedMessage(rule))
		}
		return d, status.Errorf(codes.Internal, "authorize: %v", err)
	}
	d.Scope = scope

	if scope.TenantWide() {
		d.Reason = fmt.Sprintf(
			"this request is about the whole estate and the caller holds no tenant-wide %s grant",
			rule.MinRole)
		return d, status.Error(codes.PermissionDenied, permissionDeniedMessage(rule))
	}

	// 2. A grant on exactly this instance or exactly this environment.
	best := bestRank(p.Grants, func(gr authn.Grant) bool {
		return (scope.InstanceID != "" && gr.InstanceID == scope.InstanceID) ||
			(scope.EnvironmentID != "" && gr.EnvironmentID == scope.EnvironmentID)
	})

	// 3. An environment grant covering the instance this request names. Resolved only when the
	//    caller actually holds an environment-scoped grant, so the common cases pay for no query.
	if best < required && scope.InstanceID != "" && holdsEnvironmentGrant(p.Grants) {
		environmentID, err := environmentOf(ctx, g.pool, p.TenantID, scope.InstanceID)
		switch {
		case errors.Is(err, ErrScopeUnresolvable):
			d.Reason = "the instance named does not exist in this tenant"
			return d, status.Error(codes.PermissionDenied, permissionDeniedMessage(rule))
		case err != nil:
			return d, status.Errorf(codes.Internal, "authorize: %v", err)
		}
		d.Scope.EnvironmentID = environmentID
		if r := bestRank(p.Grants, func(gr authn.Grant) bool {
			return gr.EnvironmentID != "" && gr.EnvironmentID == environmentID
		}); r > best {
			best = r
		}
	}

	if best >= required {
		d.Allowed = true
		d.EffectiveRole = roleAtLeast(p.Grants, best)
		return d, nil
	}

	d.EffectiveRole = roleAtLeast(p.Grants, best)
	d.Reason = fmt.Sprintf("caller holds %s here and %s is required",
		orNone(d.EffectiveRole), rule.MinRole)
	return d, status.Error(codes.PermissionDenied, permissionDeniedMessage(rule))
}

// permissionDeniedMessage is what the client is told, and it is the same sentence for every way of
// being refused.
//
// A refusal never says whether the resource exists, whether the caller's role was close, or which
// of several checks failed. All of those are in the server's log and in the audit record; none of
// them is a client's business, because each one is a probe an unauthorized caller could repeat.
func permissionDeniedMessage(rule Rule) string {
	if rule.MinRole == "" {
		return "permission denied"
	}
	return fmt.Sprintf("permission denied: this action requires the %s role within its scope", rule.MinRole)
}

func orNone(role string) string {
	if role == "" {
		return "no role"
	}
	return "the " + role + " role"
}

// bestRank returns the highest rank among the grants matching a predicate, or -1 for none.
//
// The maximum rather than the most specific, which is ADR-0034 and the sharpest edge in this
// package. `role_grants_single_scope` means a grant is tenant-wide, environment-wide or
// instance-wide and never two at once, so a caller can hold `dba` on an environment and `viewer` on
// one instance inside it. Under "most specific wins" that pairing would *demote* them on that
// instance — turning role_grants into a deny mechanism the schema has no column to express, and one
// that would silently stop working the day somebody added a second grant. Grants are additive: a
// grant only ever adds permission.
func bestRank(grants []authn.Grant, match func(authn.Grant) bool) int {
	best := -1
	for _, g := range grants {
		if match(g) && g.Rank > best {
			best = g.Rank
		}
	}
	return best
}

// roleAtLeast names the role holding a given rank among a caller's grants.
func roleAtLeast(grants []authn.Grant, rank int) string {
	for _, g := range grants {
		if g.Rank == rank {
			return g.Role
		}
	}
	return ""
}

func holdsEnvironmentGrant(grants []authn.Grant) bool {
	for _, g := range grants {
		if g.EnvironmentID != "" {
			return true
		}
	}
	return false
}

// highestRole is the strongest role a caller holds anywhere, which is what a UI renders beside a
// name. It is not permission to do anything in particular: scope decides that, per request.
func highestRole(grants []authn.Grant) string {
	best, role := -1, ""
	for _, g := range grants {
		if g.Rank > best {
			best, role = g.Rank, g.Role
		}
	}
	return role
}

// HighestRole is the exported form, for GetMe.
func HighestRole(grants []authn.Grant) string { return highestRole(grants) }

// resourceID names what an action acted on, for the audit record.
func resourceID(rule Rule, req proto.Message) string {
	for _, field := range []string{"instance_id", "backup_id", "schedule_id", "verification_id", "token_id", "environment_id"} {
		if id := stringField(req, field); id != "" {
			return id
		}
	}
	return ""
}
