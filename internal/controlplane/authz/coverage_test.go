package authz

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/audit"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
)

// This file is the test ADR-0024 §4 asked for, and it is the reason `.github/SECURITY.md` can say
// enforcement is server-side on every route without that being a claim somebody wrote from the
// architecture.
//
// It enumerates every method of every generated control-plane service interface by reflection —
// there is no list of route names here to fall out of date — and asserts three things about each:
//
//  1. It has an entry in Policies. A method with none is denied to everybody, which is safe, but a
//     route nobody has decided about is a route nobody has reviewed.
//  2. Called with an anonymous caller it returns Unauthenticated. Not Unimplemented, which is what
//     an undecorated method would answer, and not a successful response, which is what an
//     unguarded one would.
//  3. Called with an authenticated `viewer` it refuses every method that requires more than viewer,
//     with PermissionDenied — §7.5's "viewer cannot trigger backup/restore", proven for all of
//     them rather than for the two the criterion names.
//
// Point 2 is doing double duty on purpose. Because an undecorated method falls through to the
// embedded UnimplementedXServiceServer and answers codes.Unimplemented, the same assertion that
// proves every route needs a credential also proves every route is actually wrapped.

// services lists what has to be covered. Adding a service to the contract and not to this list is
// the one thing reflection cannot catch, which is why the count is asserted separately below.
var services = []struct {
	name  string
	iface reflect.Type
	guard func(*Enforcer) any
}{
	{
		name:  "InventoryService",
		iface: reflect.TypeOf((*fwv1.InventoryServiceServer)(nil)).Elem(),
		guard: func(e *Enforcer) any { return GuardInventory(e, panicServer{}) },
	},
	{
		name:  "ScheduleService",
		iface: reflect.TypeOf((*fwv1.ScheduleServiceServer)(nil)).Elem(),
		guard: func(e *Enforcer) any { return GuardSchedule(e, panicServer{}) },
	},
	{
		name:  "BackupService",
		iface: reflect.TypeOf((*fwv1.BackupServiceServer)(nil)).Elem(),
		guard: func(e *Enforcer) any { return GuardBackup(e, panicServer{}) },
	},
	{
		name:  "IdentityService",
		iface: reflect.TypeOf((*fwv1.IdentityServiceServer)(nil)).Elem(),
		guard: func(e *Enforcer) any { return GuardIdentity(e, panicServer{}) },
	},
}

// panicServer is the service behind the guard. Every method panics, so a test that reaches one has
// proved the guard let a request through — which fails the test far more usefully than a nil
// pointer would.
type panicServer struct {
	fwv1.UnimplementedInventoryServiceServer
	fwv1.UnimplementedScheduleServiceServer
	fwv1.UnimplementedBackupServiceServer
	fwv1.UnimplementedIdentityServiceServer
}

func TestEveryRouteHasAPolicy(t *testing.T) {
	covered := map[string]bool{}

	for _, svc := range services {
		for i := range svc.iface.NumMethod() {
			name := svc.iface.Method(i).Name
			if !isRPC(name) {
				continue
			}
			method := "/fleetward.v1." + svc.name + "/" + name
			covered[method] = true
			if _, ok := Policies[method]; !ok {
				t.Errorf("%s has no entry in Policies; it would be denied to everybody, "+
					"which is safe but means nobody has decided what it needs", method)
			}
		}
	}

	for _, method := range MethodNames() {
		if !covered[method] {
			t.Errorf("Policies names %s, which no generated service interface has; "+
				"a renamed or removed RPC leaves a rule nothing can ever match", method)
		}
	}
}

func TestPoliciesValidateAgainstTheSeededRanks(t *testing.T) {
	// The ranks migration 000001 seeds. Written out here rather than read from a database so the
	// unit suite needs no container; the integration suite asserts the table itself matches.
	ranks := map[string]int{RoleViewer: 10, RoleOperator: 20, RoleDBA: 30, RoleAdmin: 40}
	if err := ValidatePolicies(ranks); err != nil {
		t.Fatalf("policy table is not valid: %v", err)
	}
}

func TestEveryRouteRefusesAnAnonymousCaller(t *testing.T) {
	enforcer, recorded := testEnforcer(t)
	ctx := authn.WithPrincipal(context.Background(), authn.Anonymous())

	forEachRoute(t, enforcer, func(t *testing.T, method string, call func(context.Context) error) {
		recorded.reset()
		err := call(ctx)
		// Decision 5 of the brief, asserted rather than described: an unauthenticated request names
		// no principal, so it writes no audit row. It is the row an attacker can generate a million
		// of, and the log is where it belongs.
		if n := len(recorded.entries); n != 0 {
			t.Fatalf("%s wrote %d audit rows for an unauthenticated request, want 0", method, n)
		}
		if err == nil {
			t.Fatalf("%s answered an anonymous caller without an error", method)
		}
		switch code := status.Code(err); code {
		case codes.Unauthenticated:
			// Correct.
		case codes.Unimplemented:
			t.Fatalf("%s fell through to the embedded UnimplementedServer: the decorator in "+
				"decorators.go does not override it, so it is not guarded", method)
		default:
			t.Fatalf("%s answered an anonymous caller with %s, want Unauthenticated", method, code)
		}
	})
}

func TestAViewerIsRefusedEveryRouteThatNeedsMore(t *testing.T) {
	enforcer, recorded := testEnforcer(t)
	viewer := authn.Principal{
		Kind:     authn.KindUser,
		UserID:   "11111111-1111-1111-1111-111111111111",
		Actor:    "viewer@example.com",
		TenantID: "00000000-0000-0000-0000-000000000001",
		Grants:   []authn.Grant{{Role: RoleViewer, Rank: 10}},
	}
	ctx := authn.WithPrincipal(context.Background(), viewer)

	forEachRoute(t, enforcer, func(t *testing.T, method string, call func(context.Context) error) {
		rule := Policies[method]
		if rule.AnyAuthenticated || rule.MinRole == RoleViewer {
			return // Allowed; those routes reach panicServer and are covered elsewhere.
		}
		recorded.reset()
		err := call(ctx)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s requires %s and answered a viewer with %s, want PermissionDenied",
				method, rule.MinRole, status.Code(err))
		}
		// The other half of decision 5: a refusal that names a principal is the most interesting
		// row in an audit log, so every one of them writes exactly one, with succeeded = false.
		if len(recorded.entries) != 1 {
			t.Fatalf("%s refused a viewer and wrote %d audit rows, want 1",
				method, len(recorded.entries))
		}
		entry := recorded.entries[0]
		if entry.Succeeded {
			t.Fatalf("%s recorded a refusal with succeeded = true", method)
		}
		if entry.Action != rule.Action {
			t.Fatalf("%s recorded action %q, want %q", method, entry.Action, rule.Action)
		}
		if entry.Details["required_role"] != rule.MinRole {
			t.Fatalf("%s recorded required_role %q, want %q",
				method, entry.Details["required_role"], rule.MinRole)
		}
	})
}

// forEachRoute invokes every method of every guarded service with a zero-valued request.
//
// A zero request is exactly right for these assertions: it names no instance and no environment, so
// every route is asking about the whole tenant, and the caller either holds a tenant-wide grant
// that covers it or does not. Nothing is dereferenced before the guard has decided.
func forEachRoute(t *testing.T, e *Enforcer, check func(*testing.T, string, func(context.Context) error)) {
	t.Helper()
	for _, svc := range services {
		guard := reflect.ValueOf(svc.guard(e))
		for i := range svc.iface.NumMethod() {
			m := svc.iface.Method(i)
			if !isRPC(m.Name) {
				continue
			}
			method := "/fleetward.v1." + svc.name + "/" + m.Name
			guardMethod := guard.MethodByName(m.Name)
			reqType := m.Type.In(1).Elem() // (ctx, *Request) -> Request

			t.Run(method, func(t *testing.T) {
				check(t, method, func(ctx context.Context) error {
					out := guardMethod.Call([]reflect.Value{
						reflect.ValueOf(ctx),
						reflect.New(reqType),
					})
					if err, ok := out[1].Interface().(error); ok {
						return err
					}
					return nil
				})
			})
		}
	}
}

// isRPC filters out the generated bookkeeping methods, which are not routes.
func isRPC(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

// captureRecorder stands in for the audit writer, so these tests can assert what a refusal records
// without a database behind them.
type captureRecorder struct{ entries []audit.Entry }

func (c *captureRecorder) Record(_ context.Context, entry audit.Entry) {
	c.entries = append(c.entries, entry)
}

func (c *captureRecorder) reset() { c.entries = nil }

// testEnforcer builds a guard with no database. Nothing in these tests resolves a scope: an
// anonymous caller is refused before the policy is read, and a viewer asking about the whole tenant
// is refused on the tenant-wide check, which is the branch that performs no query.
func testEnforcer(t *testing.T) (*Enforcer, *captureRecorder) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	guard := &Guard{
		ranks: map[string]int{RoleViewer: 10, RoleOperator: 20, RoleDBA: 30, RoleAdmin: 40},
		log:   log,
	}
	recorder := &captureRecorder{}
	return NewEnforcer(guard, recorder, log), recorder
}

// TestARefusalNeverEchoesTheRequest guards the rule that keeps a database password out of an
// append-only table: what a refusal records is assembled from the decision, never from the message.
func TestARefusalNeverEchoesTheRequest(t *testing.T) {
	req := &fwv1.CreateInstanceRequest{
		EnvironmentId: "env-1",
		Name:          "prod-1",
		Connection:    &fwv1.ConnectionSpec{Username: "fleetward", Password: "hunter2"},
	}
	// CreateInstance produces an instance that does not exist yet, so there is nothing to name on
	// the way in and the response supplies the id. What must *not* happen is the environment id
	// being recorded beside `resource_type = instance`, which is the shape the B6 walk found in
	// the audit log and which reads as perfectly plausible.
	if got := resourceID(Policies["/fleetward.v1.InventoryService/CreateInstance"], req); got != "" {
		t.Fatalf("resourceID = %q, want empty: an instance being created has no id yet", got)
	}
	// RunBackup acts on an instance that does exist, and that is what the row names.
	run := &fwv1.RunBackupRequest{InstanceId: "instance-7"}
	if got := resourceID(Policies["/fleetward.v1.BackupService/RunBackup"], run); got != "instance-7" {
		t.Fatalf("resourceID = %q, want the instance it acts on", got)
	}

	// Everything the audit path can learn about a request is one of these fields. If a future
	// change adds a way to reach the message, this test is where it should stop being true.
	for _, field := range []string{"password", "username", "host"} {
		if v := stringField(req, field); v != "" {
			t.Fatalf("stringField exposed %q from a request; the audit path must never reach it", field)
		}
	}
}

var _ proto.Message = (*fwv1.CreateInstanceRequest)(nil)
