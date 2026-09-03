package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Scope is what a request acts on. Both fields empty means the whole tenant.
type Scope struct {
	InstanceID    string
	EnvironmentID string
}

// TenantWide reports whether this request is about the estate rather than one part of it.
func (s Scope) TenantWide() bool { return s.InstanceID == "" && s.EnvironmentID == "" }

// ErrScopeUnresolvable reports an id that names nothing in this tenant. It is deliberately not
// surfaced as "not found": a caller with no grant covering a resource must not be able to learn
// whether that resource exists by watching which error comes back.
var ErrScopeUnresolvable = errors.New("scope cannot be resolved")

// resolveScope reads the request's scope, following an id to its instance where it has to.
//
// The reads are the reason ScopeSource exists rather than a per-RPC function: `backup_id`,
// `schedule_id` and `verification_id` all name something whose scope is the instance behind them,
// and that lookup is the same three lines every time.
func (g *Guard) resolveScope(ctx context.Context, rule Rule, req proto.Message, tenantID string) (Scope, error) {
	switch rule.Scope {
	case ScopeTenant:
		return Scope{}, nil

	case ScopeRequestInstance:
		return Scope{InstanceID: stringField(req, "instance_id")}, nil

	case ScopeRequestEnvironment:
		return Scope{EnvironmentID: stringField(req, "environment_id")}, nil

	case ScopeRequestInstanceOrEnvironment:
		// The instance is the narrower of the two, so it wins when a caller supplied both.
		if id := stringField(req, "instance_id"); id != "" {
			return Scope{InstanceID: id}, nil
		}
		return Scope{EnvironmentID: stringField(req, "environment_id")}, nil

	case ScopeBackup:
		return g.instanceOf(ctx, tenantID, `
			SELECT instance_id::text FROM backups WHERE id = $1 AND tenant_id = $2`,
			stringField(req, "backup_id"))

	case ScopeSchedule:
		return g.instanceOf(ctx, tenantID, `
			SELECT instance_id::text FROM schedules WHERE id = $1 AND tenant_id = $2`,
			stringField(req, "schedule_id"))

	case ScopeVerification:
		return g.instanceOf(ctx, tenantID, `
			SELECT b.instance_id::text
			FROM   verifications v JOIN backups b ON b.id = v.backup_id
			WHERE  v.id = $1 AND v.tenant_id = $2`,
			stringField(req, "verification_id"))

	default:
		return Scope{}, fmt.Errorf("authz: unknown scope source %d", rule.Scope)
	}
}

// instanceOf runs one of the id-to-instance lookups above.
func (g *Guard) instanceOf(ctx context.Context, tenantID, query, id string) (Scope, error) {
	if id == "" {
		// A request that names nothing is asking about the tenant, and only a tenant-wide grant
		// covers that. The caller has already failed that check by the time it gets here.
		return Scope{}, nil
	}
	var instanceID string
	err := g.pool.QueryRow(ctx, query, id, tenantID).Scan(&instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, ErrScopeUnresolvable
	}
	if err != nil {
		return Scope{}, fmt.Errorf("resolve scope: %w", err)
	}
	return Scope{InstanceID: instanceID}, nil
}

// environmentOf returns the environment an instance sits in.
//
// Called only when the caller actually holds an environment-scoped grant. A caller with a
// tenant-wide grant has already been allowed, and one with only instance-scoped grants does not
// need it — so the common cases, which are every request the estate view makes, pay for no query
// at all.
func environmentOf(ctx context.Context, pool *pgxpool.Pool, tenantID, instanceID string) (string, error) {
	var environmentID string
	err := pool.QueryRow(ctx,
		`SELECT environment_id::text FROM instances WHERE id = $1 AND tenant_id = $2`,
		instanceID, tenantID).Scan(&environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrScopeUnresolvable
	}
	if err != nil {
		return "", fmt.Errorf("resolve instance environment: %w", err)
	}
	return environmentID, nil
}

// stringField reads a string field from a request message by name.
//
// Protobuf reflection rather than a type switch over 24 request types: the field names are part of
// the contract, a message that does not have one simply yields the empty string, and there is no
// list of types to forget to extend when the contract grows.
func stringField(msg proto.Message, name string) string {
	if msg == nil {
		return ""
	}
	m := msg.ProtoReflect()
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil || fd.Kind() != protoreflect.StringKind || fd.IsList() {
		return ""
	}
	return m.Get(fd).String()
}
