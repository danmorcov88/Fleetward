//go:build integration

// Integration tests for the PostgreSQL plugin, run against a real server via testcontainers.
//
// A backup and monitoring tool that has only been tested against mocks has not been tested
// (ADR-0012). These require Docker and no pre-installed PostgreSQL — a fresh clone plus a container
// runtime is sufficient.
//
// Run with: go test -tags=integration ./plugins/postgres/...
package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
)

const (
	testDB       = "fleetward_test"
	testUser     = "fleetward"
	testPass     = "fleetward-integration"
	testImage    = "postgres:16-alpine"
	startTimeout = 3 * time.Minute
)

// startPostgres brings up a real PostgreSQL and returns credentials pointing at it.
func startPostgres(t *testing.T) *fwv1.Credentials {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	container, err := postgres.Run(ctx, testImage,
		postgres.WithDatabase(testDB),
		postgres.WithUsername(testUser),
		postgres.WithPassword(testPass),
		testcontainers.WithWaitStrategy(
			// Postgres restarts once during initdb, so the log line appears twice. Waiting for the
			// second occurrence avoids connecting to a server that is about to go away.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startTimeout)),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	return &fwv1.Credentials{
		Host: host,
		// Num returns uint16, so widening to int32 is always safe.
		Port:     int32(port.Num()),
		Username: testUser,
		Password: testPass,
		Database: testDB,
	}
}

// seed creates tables so discovery has something non-trivial to find.
func seed(t *testing.T, creds *fwv1.Credentials) {
	t.Helper()

	ctx := context.Background()
	conn, err := connect(ctx, creds)
	if err != nil {
		t.Fatalf("connect for seeding: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	for _, stmt := range []string{
		`CREATE TABLE customers (id serial PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE orders (id serial PRIMARY KEY, customer_id int REFERENCES customers(id), total numeric)`,
		`INSERT INTO customers (name) SELECT 'customer-' || g FROM generate_series(1, 40) g`,
		`INSERT INTO orders (customer_id, total) SELECT (g % 40) + 1, g * 1.5 FROM generate_series(1, 120) g`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

func TestHealthCheckAgainstRealPostgres(t *testing.T) {
	creds := startPostgres(t)
	ctx := context.Background()

	status, err := New().HealthCheck(ctx, &fwv1.HealthCheckRequest{Credentials: creds})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	if status.GetState() != fwv1.HealthState_HEALTH_STATE_UP {
		t.Fatalf("state = %v (%s), want UP", status.GetState(), status.GetMessage())
	}
	if status.GetEngineVersion() == "" {
		t.Error("engine_version is empty")
	}
	if status.GetLatency().AsDuration() <= 0 {
		t.Error("latency was not measured")
	}

	// A fresh single-node instance is not in recovery, so connection usage is the signal we expect.
	var sawConnectionUsage bool
	for _, s := range status.GetSignals() {
		if s.GetName() == "connection_usage" {
			sawConnectionUsage = true
			if s.GetValue() < 0 || s.GetValue() > 100 {
				t.Errorf("connection usage = %v%%, outside 0-100", s.GetValue())
			}
		}
		if s.GetName() == "in_recovery" {
			t.Error("a single-node primary should not report in_recovery")
		}
	}
	if !sawConnectionUsage {
		t.Error("no connection_usage signal was reported")
	}
}

// TestHealthCheckOnUnreachableInstance is the behaviour the contract is most explicit about: being
// down is a valid answer, not an RPC failure. Returning an error here would make core unable to
// distinguish "the database is down" from "we could not ask".
func TestHealthCheckOnUnreachableInstance(t *testing.T) {
	ctx := context.Background()

	status, err := New().HealthCheck(ctx, &fwv1.HealthCheckRequest{
		Credentials: &fwv1.Credentials{
			// Reserved for documentation, so it never resolves to a real host.
			Host: "192.0.2.1", Port: 5432, Username: "u", Password: "p", Database: "d",
		},
		Timeout: probeTimeout(3 * time.Second),
	})
	if err != nil {
		t.Fatalf("an unreachable instance must not produce an RPC error: %v", err)
	}
	if status.GetState() != fwv1.HealthState_HEALTH_STATE_DOWN {
		t.Fatalf("state = %v, want DOWN", status.GetState())
	}
	if status.GetError() == nil {
		t.Fatal("DOWN was reported without a structured error")
	}
	if !status.GetError().GetRetryable() {
		t.Error("an unreachable host should be retryable")
	}
}

func TestHealthCheckWithWrongPassword(t *testing.T) {
	creds := startPostgres(t)
	creds.Password = "definitely-not-the-password"

	status, err := New().HealthCheck(context.Background(), &fwv1.HealthCheckRequest{Credentials: creds})
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if status.GetState() != fwv1.HealthState_HEALTH_STATE_DOWN {
		t.Fatalf("state = %v, want DOWN", status.GetState())
	}

	pe := status.GetError()
	if pe.GetCode() != fwv1.ErrorCode_ERROR_CODE_AUTHENTICATION_FAILED {
		t.Errorf("code = %v, want AUTHENTICATION_FAILED", pe.GetCode())
	}
	// Retrying a wrong password only burns attempts and can trip account lockout on the monitored
	// instance.
	if pe.GetRetryable() {
		t.Error("authentication failure must not be retryable")
	}
	if strings.Contains(pe.GetMessage(), creds.GetPassword()) {
		t.Fatalf("the password leaked into the error message: %q", pe.GetMessage())
	}
}

func TestDiscoverAgainstRealPostgres(t *testing.T) {
	creds := startPostgres(t)
	seed(t, creds)
	ctx := context.Background()

	resp, err := New().Discover(ctx, &fwv1.DiscoverRequest{Credentials: creds})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	server := resp.GetServer()
	if server.GetEngineType() != EngineType {
		t.Errorf("engine_type = %q, want %q", server.GetEngineType(), EngineType)
	}
	if server.GetVersion() == "" {
		t.Error("normalized version is empty")
	}
	if server.GetVersionString() == "" {
		t.Error("raw version string is empty")
	}
	if server.GetUptime().AsDuration() <= 0 {
		t.Error("uptime was not reported")
	}
	if server.GetReadOnly() {
		t.Error("a primary reported itself read-only")
	}
	if server.GetAttributes()["max_connections"] == "" {
		t.Error("max_connections was not surfaced")
	}

	var found *fwv1.DatabaseInfo
	for _, db := range resp.GetDatabases() {
		if db.GetName() == testDB {
			found = db
		}
		if db.GetName() == "template0" {
			t.Error("template0 was listed; it cannot be connected to")
		}
	}
	if found == nil {
		t.Fatalf("database %q was not discovered", testDB)
	}
	if found.GetSizeBytes() <= 0 {
		t.Error("database size was not reported")
	}
	if found.GetOwner() != testUser {
		t.Errorf("owner = %q, want %q", found.GetOwner(), testUser)
	}
	if found.GetIsSystem() {
		t.Error("the application database was marked as a system database")
	}

	// System databases must be identified so the UI can de-emphasize them and backups can skip them.
	var sawSystem bool
	for _, db := range resp.GetDatabases() {
		if db.GetName() == "postgres" && db.GetIsSystem() {
			sawSystem = true
		}
	}
	if !sawSystem {
		t.Error("the 'postgres' database was not marked as a system database")
	}

	topology := resp.GetTopology()
	if len(topology.GetNodes()) != 1 {
		t.Fatalf("nodes = %d, want 1 for a standalone instance", len(topology.GetNodes()))
	}
	self := topology.GetNodes()[0]
	if !self.GetIsSelf() {
		t.Error("the only node is not marked is_self")
	}
	if self.GetRole() != fwv1.NodeRole_NODE_ROLE_PRIMARY {
		t.Errorf("role = %v, want PRIMARY", self.GetRole())
	}
	if !topology.GetIsStandalone() {
		t.Error("a single instance with no replicas should be standalone")
	}
}

func TestDiscoverSkipDatabaseDetails(t *testing.T) {
	creds := startPostgres(t)
	ctx := context.Background()

	resp, err := New().Discover(ctx, &fwv1.DiscoverRequest{
		Credentials:         creds,
		SkipDatabaseDetails: true,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// Enumerating sizes is expensive on a large estate, so the caller must be able to opt out.
	if len(resp.GetDatabases()) != 0 {
		t.Errorf("databases = %d, want 0 when details are skipped", len(resp.GetDatabases()))
	}
	if resp.GetServer() == nil {
		t.Error("server info should still be reported")
	}
}

func TestDiscoverOnUnreachableInstanceFails(t *testing.T) {
	// Unlike HealthCheck, Discover has no useful answer for an unreachable instance: the caller
	// asked for an inventory and there is none.
	_, err := New().Discover(context.Background(), &fwv1.DiscoverRequest{
		Credentials: &fwv1.Credentials{Host: "192.0.2.1", Port: 5432, Username: "u", Password: "p"},
	})
	if err == nil {
		t.Fatal("expected an error for an unreachable instance")
	}
	if pe := sdk.AsPluginError(err); pe.GetCode() != fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED {
		t.Errorf("code = %v, want CONNECTION_FAILED", pe.GetCode())
	}
}

// probeTimeout bounds a health check against a host that will never answer, so the test fails in
// seconds rather than waiting out the default.
func probeTimeout(d time.Duration) *durationpb.Duration { return durationpb.New(d) }
