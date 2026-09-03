//go:build integration

// Integration tests for the authorization spine against a real metadata store.
//
// The unit tests in coverage_test.go prove the shape of the thing: every route has a policy, every
// route refuses an anonymous caller, every route that needs more than `viewer` refuses one. They do
// it with no database, which is why they can prove those and nothing else.
//
// What needs a real Postgres is everything that depends on rows: the seeded role ranks actually
// matching what the policy table names, a grant on one instance not carrying to its neighbour, the
// additive resolution rule that ADR-0034 chose over the more obvious one, and the append-only
// trigger that makes the audit log evidence rather than a log.
//
// Requires Docker and no pre-installed PostgreSQL.
//
// Run with: go test -tags=integration ./internal/controlplane/authz/...
package authz

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/audit"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

const (
	metaImage    = "postgres:16-alpine"
	metaDB       = "fleetward"
	metaUser     = "fleetward"
	metaPass     = "fleetward-integration"
	startTimeout = 3 * time.Minute
)

type harness struct {
	pool  *pgxpool.Pool
	guard *Guard
	audit *audit.Writer
	log   *slog.Logger
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	container, err := postgres.Run(ctx, metaImage,
		postgres.WithDatabase(metaDB),
		postgres.WithUsername(metaUser),
		postgres.WithPassword(metaPass),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startTimeout)),
	)
	if err != nil {
		t.Fatalf("start metadata postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := metadb.Open(ctx, config.MetaDBConfig{DSN: dsn, ConnectTimeout: 30 * time.Second}, log)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	guard, err := NewGuard(ctx, db.Pool(), log)
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}

	return &harness{pool: db.Pool(), guard: guard, audit: audit.NewWriter(db.Pool(), log), log: log}
}

// environment and instance seed the estate directly, because this package tests authorization and
// has no business depending on the inventory service to do it.
func (h *harness) environment(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO environments (tenant_id, name, is_production)
		VALUES ($1, $2, TRUE) RETURNING id::text`, metadb.DefaultTenantID, name).Scan(&id); err != nil {
		t.Fatalf("seed environment: %v", err)
	}
	return id
}

func (h *harness) instance(t *testing.T, environmentID, name string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, $3, 'testengine', 'localhost', 5432) RETURNING id::text`,
		metadb.DefaultTenantID, environmentID, name).Scan(&id); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return id
}

// user creates a person and returns their id.
func (h *harness) user(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO users (tenant_id, subject, email, display_name)
		VALUES ($1, $2, $2, $2) RETURNING id::text`, metadb.DefaultTenantID, email).Scan(&id); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

// grant binds a user to a role. An empty environmentID and instanceID means tenant-wide.
func (h *harness) grant(t *testing.T, userID, role, environmentID, instanceID string) {
	t.Helper()
	if _, err := h.pool.Exec(context.Background(), `
		INSERT INTO role_grants (tenant_id, user_id, role_name, environment_id, instance_id)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, NULLIF($5, '')::uuid)`,
		metadb.DefaultTenantID, userID, role, environmentID, instanceID); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// principal loads a user the way authentication does, so the tests exercise the same query.
func (h *harness) principal(t *testing.T, userID, email string) authn.Principal {
	t.Helper()
	grants, err := authn.LoadGrants(context.Background(), h.pool, metadb.DefaultTenantID, userID)
	if err != nil {
		t.Fatalf("load grants: %v", err)
	}
	return authn.Principal{
		Kind:     authn.KindUser,
		UserID:   userID,
		Actor:    email,
		Email:    email,
		TenantID: metadb.DefaultTenantID,
		Grants:   grants,
	}
}

func (h *harness) ctx(p authn.Principal) context.Context {
	return authn.WithPrincipal(context.Background(), p)
}

// -------------------------------------------------------------------------------------------------
// The ranks are facts in the database, not constants in Go
// -------------------------------------------------------------------------------------------------

func TestTheSeededRanksAreWhatThePolicyTableAssumes(t *testing.T) {
	h := newHarness(t)

	ranks := h.guard.Ranks()
	for role, want := range map[string]int{
		RoleViewer: 10, RoleOperator: 20, RoleDBA: 30, RoleAdmin: 40,
	} {
		if got := ranks[role]; got != want {
			t.Errorf("rank of %q is %d in the database, %d in this test; one of the two is wrong",
				role, got, want)
		}
	}

	// NewGuard runs this at startup, so a policy naming a role the table does not have refuses to
	// serve rather than producing one confusing 403 per request.
	if err := ValidatePolicies(ranks); err != nil {
		t.Fatalf("the policy table does not validate against the seeded roles: %v", err)
	}
}

// -------------------------------------------------------------------------------------------------
// §7.5: a viewer cannot trigger a backup, a dba can, and both attempts are recorded
// -------------------------------------------------------------------------------------------------

func TestAViewerIsRefusedABackupAndADbaIsNot(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	instance := h.instance(t, env, "prod-1")

	viewerID := h.user(t, "viewer@example.com")
	h.grant(t, viewerID, RoleViewer, "", "")
	dbaID := h.user(t, "dba@example.com")
	h.grant(t, dbaID, RoleDBA, "", "")

	req := &fwv1.RunBackupRequest{InstanceId: instance}
	const method = "/fleetward.v1.BackupService/RunBackup"

	viewer := h.principal(t, viewerID, "viewer@example.com")
	decision, err := h.guard.Check(h.ctx(viewer), method, req)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a viewer running a backup got %v, want PermissionDenied", err)
	}
	if decision.Allowed {
		t.Fatal("the decision says allowed on a refusal")
	}
	// The client is told one sentence and never which check failed; the reason is for the record.
	if strings.Contains(status.Convert(err).Message(), instance) {
		t.Error("the refusal message echoes the instance id back to the caller")
	}

	dba := h.principal(t, dbaID, "dba@example.com")
	decision, err = h.guard.Check(h.ctx(dba), method, req)
	if err != nil {
		t.Fatalf("a dba running a backup was refused: %v", err)
	}
	if !decision.Allowed || decision.EffectiveRole != RoleDBA {
		t.Fatalf("decision = %+v, want allowed as dba", decision)
	}
}

// -------------------------------------------------------------------------------------------------
// A grant on one instance does not carry to its neighbour
// -------------------------------------------------------------------------------------------------

func TestAnInstanceGrantDoesNotReachTheInstanceNextToIt(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	mine := h.instance(t, env, "prod-1")
	theirs := h.instance(t, env, "prod-2")

	userID := h.user(t, "scoped@example.com")
	h.grant(t, userID, RoleDBA, "", mine)
	p := h.principal(t, userID, "scoped@example.com")

	const method = "/fleetward.v1.BackupService/RunBackup"

	if _, err := h.guard.Check(h.ctx(p), method, &fwv1.RunBackupRequest{InstanceId: mine}); err != nil {
		t.Fatalf("refused on the instance the grant names: %v", err)
	}
	if _, err := h.guard.Check(h.ctx(p), method, &fwv1.RunBackupRequest{InstanceId: theirs}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("allowed on the neighbouring instance: %v", err)
	}

	// And the same caller cannot ask about the estate, which is the other half of the scope rule:
	// a request that names nothing is asking about the whole tenant (ADR-0035).
	if _, err := h.guard.Check(h.ctx(p), "/fleetward.v1.BackupService/ListBackups",
		&fwv1.ListBackupsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an instance-scoped caller listed the whole estate: %v", err)
	}
	// But it can ask about its own instance.
	if _, err := h.guard.Check(h.ctx(p), "/fleetward.v1.BackupService/ListBackups",
		&fwv1.ListBackupsRequest{InstanceId: mine}); err != nil {
		t.Fatalf("refused a listing scoped to the caller's own instance: %v", err)
	}
}

// -------------------------------------------------------------------------------------------------
// ADR-0034, and the pairing that is the reason it exists
// -------------------------------------------------------------------------------------------------

func TestGrantsAddUpAndNeverSubtract(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "staging")
	target := h.instance(t, env, "staging-3")
	other := h.instance(t, env, "staging-4")

	const runBackup = "/fleetward.v1.BackupService/RunBackup"

	t.Run("an instance grant elevates inside a weaker environment grant", func(t *testing.T) {
		userID := h.user(t, "elevated@example.com")
		h.grant(t, userID, RoleViewer, env, "")
		h.grant(t, userID, RoleDBA, "", target)
		p := h.principal(t, userID, "elevated@example.com")

		if _, err := h.guard.Check(h.ctx(p), runBackup, &fwv1.RunBackupRequest{InstanceId: target}); err != nil {
			t.Fatalf("the instance grant did not elevate: %v", err)
		}
		if _, err := h.guard.Check(h.ctx(p), runBackup, &fwv1.RunBackupRequest{InstanceId: other}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("the elevation leaked to another instance: %v", err)
		}
	})

	// This is the case ADR-0034 turns on. Under "most specific wins" — the rule most people expect —
	// the narrow viewer grant would *demote* this caller on staging-3, turning role_grants into a
	// deny mechanism the schema has no column to express.
	t.Run("an instance grant never demotes a stronger environment grant", func(t *testing.T) {
		userID := h.user(t, "notdemoted@example.com")
		h.grant(t, userID, RoleDBA, env, "")
		h.grant(t, userID, RoleViewer, "", target)
		p := h.principal(t, userID, "notdemoted@example.com")

		if _, err := h.guard.Check(h.ctx(p), runBackup, &fwv1.RunBackupRequest{InstanceId: target}); err != nil {
			t.Fatalf("a viewer grant on one instance demoted a dba grant on its environment: %v\n"+
				"grants add up; see ADR-0034", err)
		}
	})

	t.Run("an environment grant reaches the instances inside it", func(t *testing.T) {
		userID := h.user(t, "envwide@example.com")
		h.grant(t, userID, RoleDBA, env, "")
		p := h.principal(t, userID, "envwide@example.com")

		if _, err := h.guard.Check(h.ctx(p), runBackup, &fwv1.RunBackupRequest{InstanceId: other}); err != nil {
			t.Fatalf("an environment grant did not reach an instance inside it: %v", err)
		}
	})
}

// -------------------------------------------------------------------------------------------------
// Resolving an id to its instance, and refusing to say whether it exists
// -------------------------------------------------------------------------------------------------

func TestAnUnknownResourceIsRefusedRatherThanReportedMissing(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	instance := h.instance(t, env, "prod-1")

	userID := h.user(t, "scoped@example.com")
	h.grant(t, userID, RoleDBA, "", instance)
	p := h.principal(t, userID, "scoped@example.com")

	// A backup that does not exist. A caller with no grant covering it must not be able to learn
	// whether it is missing or merely forbidden by watching which error comes back.
	_, err := h.guard.Check(h.ctx(p), "/fleetward.v1.BackupService/GetBackup",
		&fwv1.GetBackupRequest{BackupId: "00000000-0000-0000-0000-0000000000ff"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unknown backup produced %v, want PermissionDenied", err)
	}
}

// -------------------------------------------------------------------------------------------------
// The audit log
// -------------------------------------------------------------------------------------------------

func TestARefusalIsRecordedAndAnUnauthenticatedRequestIsNot(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	instance := h.instance(t, env, "prod-1")

	viewerID := h.user(t, "viewer@example.com")
	h.grant(t, viewerID, RoleViewer, "", "")
	viewer := h.principal(t, viewerID, "viewer@example.com")

	enforcer := NewEnforcer(h.guard, h.audit, h.log)
	guarded := GuardBackup(enforcer, &refusingBackupService{})

	// A refused viewer.
	if _, err := guarded.RunBackup(h.ctx(viewer), &fwv1.RunBackupRequest{InstanceId: instance}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RunBackup as viewer: %v", err)
	}

	// An anonymous caller.
	anon := authn.WithPrincipal(context.Background(), authn.Anonymous())
	if _, err := guarded.RunBackup(anon, &fwv1.RunBackupRequest{InstanceId: instance}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("RunBackup anonymously: %v", err)
	}

	rows := h.auditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit_log holds %d rows, want exactly 1: the refusal of somebody who is somebody, "+
			"and nothing for the request that named nobody", len(rows))
	}
	row := rows[0]
	switch {
	case row.actor != "viewer@example.com":
		t.Errorf("actor = %q, want the viewer's email", row.actor)
	case row.userID != viewerID:
		t.Errorf("user_id = %q, want the viewer's id", row.userID)
	case row.action != "backup.run":
		t.Errorf("action = %q, want backup.run", row.action)
	case row.succeeded:
		t.Error("succeeded = true on a refusal")
	case row.resourceID != instance:
		t.Errorf("resource_id = %q, want the instance", row.resourceID)
	}
	if row.details["required_role"] != RoleDBA {
		t.Errorf("details.required_role = %q, want dba", row.details["required_role"])
	}
	if row.details["effective_role"] != RoleViewer {
		t.Errorf("details.effective_role = %q, want viewer", row.details["effective_role"])
	}
}

// TestASystemCallerIsRecordedWithNoUser is ADR-0036 asserted rather than described: two thirds of
// what Fleetward does has nobody behind it, and the audit log has to name it anyway.
func TestASystemCallerIsRecordedWithNoUser(t *testing.T) {
	h := newHarness(t)

	ctx := authn.WithPrincipal(context.Background(),
		authn.System("retention", metadb.DefaultTenantID))
	h.audit.Record(ctx, audit.Entry{
		Action:       "backup.expire",
		ResourceType: "backup",
		ResourceID:   "00000000-0000-0000-0000-0000000000aa",
		Succeeded:    true,
	})

	rows := h.auditRows(t)
	if len(rows) != 1 {
		t.Fatalf("audit_log holds %d rows, want 1", len(rows))
	}
	if rows[0].actor != "system:retention" {
		t.Errorf(`actor = %q, want "system:retention" — the answer to "who deleted this artifact"`,
			rows[0].actor)
	}
	if rows[0].userID != "" {
		t.Errorf("user_id = %q, want NULL: a system caller has no row in users", rows[0].userID)
	}
}

// TestTheAuditLogRefusesToBeEdited is not testing our code. It is testing that migration 000001's
// trigger is still there, because everything the audit log is for rests on it — and because a test
// that tried to tidy up after itself would discover it the hard way, in CI.
func TestTheAuditLogRefusesToBeEdited(t *testing.T) {
	h := newHarness(t)
	ctx := authn.WithPrincipal(context.Background(),
		authn.System("test", metadb.DefaultTenantID))
	h.audit.Record(ctx, audit.Entry{Action: "test.write", ResourceType: "test", Succeeded: true})

	if _, err := h.pool.Exec(ctx, `UPDATE audit_log SET succeeded = FALSE`); err == nil {
		t.Fatal("UPDATE on audit_log succeeded; the append-only trigger is gone")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("UPDATE failed for the wrong reason: %v", err)
	}

	if _, err := h.pool.Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE on audit_log succeeded; the append-only trigger is gone")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE failed for the wrong reason: %v", err)
	}

	// TRUNCATE is not blocked — the trigger is FOR EACH ROW — which is the only reason a test
	// harness can reset this table at all. Worth asserting so nobody "fixes" the trigger to cover
	// it and breaks every suite that resets.
	if _, err := h.pool.Exec(ctx, `TRUNCATE audit_log`); err != nil {
		t.Fatalf("TRUNCATE on audit_log failed: %v", err)
	}
}

type auditRow struct {
	actor        string
	userID       string
	action       string
	resourceType string
	resourceID   string
	succeeded    bool
	details      map[string]string
}

func (h *harness) auditRows(t *testing.T) []auditRow {
	t.Helper()
	rows, err := h.pool.Query(context.Background(), `
		SELECT actor, COALESCE(user_id::text, ''), action, resource_type, resource_id, succeeded, details
		FROM   audit_log ORDER BY id`)
	if err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	defer rows.Close()

	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.actor, &r.userID, &r.action, &r.resourceType, &r.resourceID,
			&r.succeeded, &r.details); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	return out
}

// refusingBackupService stands behind the guard. Reaching it at all means the guard let something
// through, which is why every method says so rather than returning a plausible response.
type refusingBackupService struct {
	fwv1.UnimplementedBackupServiceServer
}

func (s *refusingBackupService) RunBackup(context.Context, *fwv1.RunBackupRequest) (*fwv1.RunBackupResponse, error) {
	return nil, errors.New("the guard let an unauthorized request through to the service")
}

// -------------------------------------------------------------------------------------------------
// A listing that filters its own rows
// -------------------------------------------------------------------------------------------------

// TestAScopedCallerMayListAndSeesOnlyItsOwn is the fix for what the B6 walk found: "a scoped grant
// cannot enumerate the estate" meant a person granted `dba` on their three servers could not address
// any of them, because the CLI resolves an instance name by listing and the estate view *is* a
// listing.
//
// The guard's part is asserted here; that the service actually filters its rows is asserted in the
// inventory suite, because a flag saying "this one filters" is worth nothing if the query does not.
func TestAScopedCallerMayListAndSeesOnlyItsOwn(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	mine := h.instance(t, env, "prod-1")
	h.instance(t, env, "prod-2")

	userID := h.user(t, "scoped@example.com")
	h.grant(t, userID, RoleDBA, "", mine)
	p := h.principal(t, userID, "scoped@example.com")

	for _, method := range []string{
		"/fleetward.v1.InventoryService/ListInstances",
		"/fleetward.v1.BackupService/GetBackupAdherence",
	} {
		if !Policies[method].ScopeFiltered {
			t.Fatalf("%s is expected to filter its own rows and is not marked ScopeFiltered", method)
		}
	}

	if _, err := h.guard.Check(h.ctx(p), "/fleetward.v1.InventoryService/ListInstances",
		&fwv1.ListInstancesRequest{}); err != nil {
		t.Fatalf("a scoped caller was refused a listing it is allowed to see part of: %v", err)
	}
	if _, err := h.guard.Check(h.ctx(p), "/fleetward.v1.BackupService/GetBackupAdherence",
		&fwv1.GetBackupAdherenceRequest{}); err != nil {
		t.Fatalf("a scoped caller was refused the estate view: %v", err)
	}

	// A listing that does *not* filter still needs the estate, because answering it in full would
	// hand a scoped caller rows they may not see.
	if _, err := h.guard.Check(h.ctx(p), "/fleetward.v1.BackupService/ListBackups",
		&fwv1.ListBackupsRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an unfiltered listing answered a scoped caller: %v", err)
	}
}

// TestVisibilityIsWhatTheGrantsCover pins the filter itself, which is what the queries apply.
func TestVisibilityIsWhatTheGrantsCover(t *testing.T) {
	h := newHarness(t)
	env := h.environment(t, "production")
	mine := h.instance(t, env, "prod-1")

	t.Run("a tenant-wide grant sees everything", func(t *testing.T) {
		id := h.user(t, "wide@example.com")
		h.grant(t, id, RoleViewer, "", "")
		v := authn.VisibilityFor(h.ctx(h.principal(t, id, "wide@example.com")), 0)
		if !v.All {
			t.Fatal("a tenant-wide grant did not produce Visibility.All")
		}
	})

	t.Run("a scoped grant sees its scopes", func(t *testing.T) {
		id := h.user(t, "narrow@example.com")
		h.grant(t, id, RoleDBA, "", mine)
		h.grant(t, id, RoleViewer, env, "")
		v := authn.VisibilityFor(h.ctx(h.principal(t, id, "narrow@example.com")), 0)
		switch {
		case v.All:
			t.Fatal("a scoped grant produced Visibility.All; it would see the whole estate")
		case len(v.InstanceIDs) != 1 || v.InstanceIDs[0] != mine:
			t.Fatalf("instances = %v, want just the granted one", v.InstanceIDs)
		case len(v.EnvironmentIDs) != 1 || v.EnvironmentIDs[0] != env:
			t.Fatalf("environments = %v, want just the granted one", v.EnvironmentIDs)
		}
	})

	t.Run("no grants at all sees nothing", func(t *testing.T) {
		id := h.user(t, "nothing@example.com")
		v := authn.VisibilityFor(h.ctx(h.principal(t, id, "nothing@example.com")), 0)
		if !v.Empty() {
			t.Fatal("a caller with no grants can see something")
		}
	})

	t.Run("a system caller sees everything", func(t *testing.T) {
		ctx := authn.WithPrincipal(context.Background(),
			authn.System("scheduler", metadb.DefaultTenantID))
		if !authn.VisibilityFor(ctx, 0).All {
			t.Fatal("the scheduler cannot see the estate it schedules")
		}
	})
}
