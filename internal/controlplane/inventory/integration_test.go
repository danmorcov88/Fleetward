//go:build integration

// Integration tests for the inventory service against a real metadata store.
//
// What is under test here is the path this slice owns: store an instance, store its credentials
// safely, resolve them, hand them to a plugin, record what came back. The plugin is a stub on
// purpose — the PostgreSQL plugin is already covered against a real server by the A1 tests, and
// core's own tests staying engine-agnostic is the architectural point rather than an omission.
//
// Requires Docker and no pre-installed PostgreSQL.
//
// Run with: go test -tags=integration ./internal/controlplane/inventory/...
package inventory

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
)

const (
	metaImage    = "postgres:16-alpine"
	metaDB       = "fleetward"
	metaUser     = "fleetward"
	metaPass     = "fleetward-integration"
	startTimeout = 3 * time.Minute
	// storedPassword is the credential every test stores. It is deliberately distinctive so that a
	// grep for it across the metadata store is a meaningful leak check.
	storedPassword = "pa55word-that-must-never-appear-in-plaintext"
	engine         = "testengine"
)

// harness is one test's isolated world: a real metadata store, a real secrets provider, and a stub
// plugin.
type harness struct {
	svc    *Service
	pool   *pgxpool.Pool
	secret secrets.Provider
	plugin *stubEngine
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(testTenantCtx(), startTimeout)
	defer cancel()

	container, err := postgres.Run(ctx, metaImage,
		postgres.WithDatabase(metaDB),
		postgres.WithUsername(metaUser),
		postgres.WithPassword(metaPass),
		testcontainers.WithWaitStrategy(
			// initdb restarts the server once, so the log line appears twice. Waiting for the second
			// occurrence avoids connecting to a server that is about to go away.
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

	key, err := secrets.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	masterKey, err := secrets.LoadMasterKey(key, "")
	if err != nil {
		t.Fatalf("load master key: %v", err)
	}
	provider, err := secrets.NewAESGCM(db, masterKey, 1)
	if err != nil {
		t.Fatalf("secrets provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })

	plugin := &stubEngine{}
	return &harness{
		svc:    New(db.Pool(), provider, &stubRouter{client: plugin}, log),
		pool:   db.Pool(),
		secret: provider,
		plugin: plugin,
	}
}

// environment creates an environment and returns its identifier.
func (h *harness) environment(t *testing.T, name string) string {
	t.Helper()
	env, err := h.svc.CreateEnvironment(testTenantCtx(), CreateEnvironmentInput{
		Name:         name,
		IsProduction: true,
	})
	if err != nil {
		t.Fatalf("CreateEnvironment(%q): %v", name, err)
	}
	return env.GetId()
}

// instance adds an instance with the standard stored password.
func (h *harness) instance(t *testing.T, environmentID, name string) *fwv1.Instance {
	t.Helper()
	inst, err := h.svc.CreateInstance(testTenantCtx(), CreateInstanceInput{
		EnvironmentID: environmentID,
		Name:          name,
		EngineType:    engine,
		Host:          "db.example.internal",
		Port:          5432,
		Labels:        map[string]string{"team": "platform"},
		Connection: &fwv1.ConnectionSpec{
			Username: "fleetward",
			Password: storedPassword,
			Database: "app",
			Options:  map[string]string{"sslmode": "require"},
		},
	})
	if err != nil {
		t.Fatalf("CreateInstance(%q): %v", name, err)
	}
	return inst
}

// -----------------------------------------------------------------------------------------------
// The credential path
// -----------------------------------------------------------------------------------------------

// TestStoredPasswordIsCiphertextEverywhere is the test this slice exists to pass. A password that
// can be read out of the metadata store is the whole product's worst failure, so it is checked
// against every column that could plausibly hold one rather than only against the one we wrote.
func TestStoredPasswordIsCiphertextEverywhere(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	h.instance(t, h.environment(t, "production"), "prod-1")

	var secretCount, cipherLen int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*)::int, coalesce(max(length(ciphertext)), 0) FROM secrets`).
		Scan(&secretCount, &cipherLen); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if secretCount != 1 {
		t.Fatalf("secrets rows = %d, want 1", secretCount)
	}
	if cipherLen == 0 {
		t.Fatal("the secret was stored with an empty ciphertext")
	}

	// The plaintext must not survive anywhere: not in the ciphertext, not in the connection row that
	// deliberately holds everything else about the connection.
	for _, query := range []string{
		`SELECT count(*)::int FROM secrets WHERE position($1 in convert_from(ciphertext, 'UTF8')) > 0`,
		`SELECT count(*)::int FROM connections WHERE options::text LIKE '%' || $1 || '%'`,
		`SELECT count(*)::int FROM connections WHERE username LIKE '%' || $1 || '%'
		                                          OR database LIKE '%' || $1 || '%'
		                                          OR secret_name LIKE '%' || $1 || '%'`,
	} {
		var hits int
		// convert_from on random ciphertext can fail on invalid UTF-8, which is itself proof the
		// bytes are not our plaintext; treat that as a pass.
		if err := h.pool.QueryRow(ctx, query, storedPassword).Scan(&hits); err != nil {
			continue
		}
		if hits != 0 {
			t.Errorf("the plaintext password appears in the metadata store: %s", query)
		}
	}

	// The non-secret half is stored where an operator can audit it.
	var username, database, secretName string
	var tlsEnabled bool
	if err := h.pool.QueryRow(ctx,
		`SELECT username, database, secret_name, tls_enabled FROM connections`).
		Scan(&username, &database, &secretName, &tlsEnabled); err != nil {
		t.Fatalf("read connection: %v", err)
	}
	if username != "fleetward" || database != "app" {
		t.Errorf("connection = %q/%q, want fleetward/app", username, database)
	}
	if !strings.HasPrefix(secretName, secretNamePrefix) {
		t.Errorf("secret_name = %q, want the %q namespace", secretName, secretNamePrefix)
	}
	if tlsEnabled {
		t.Error("TLS was recorded as enabled for a connection that did not ask for it")
	}
}

// TestResolvedCredentialsReachThePluginIntact proves the round trip: what was stored encrypted is
// what the plugin is handed, for exactly one call.
func TestResolvedCredentialsReachThePluginIntact(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	if _, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{InstanceId: inst.GetId()}); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}

	got := h.plugin.lastCredentials()
	if got == nil {
		t.Fatal("the plugin was never called")
	}
	if got.GetPassword() != storedPassword {
		t.Error("the password did not survive encryption and decryption")
	}
	if got.GetUsername() != "fleetward" || got.GetDatabase() != "app" {
		t.Errorf("username/database = %q/%q", got.GetUsername(), got.GetDatabase())
	}
	if got.GetHost() != "db.example.internal" || got.GetPort() != 5432 {
		t.Errorf("endpoint = %s:%d", got.GetHost(), got.GetPort())
	}
	if got.GetOptions()["sslmode"] != "require" {
		t.Errorf("engine options were lost: %v", got.GetOptions())
	}
	// TLS was not requested, so the block must be absent rather than present and empty.
	if got.GetTls() != nil {
		t.Errorf("tls = %v, want nil when TLS is disabled", got.GetTls())
	}

	ref := h.plugin.lastConnectionRef()
	if ref.GetInstanceId() != inst.GetId() {
		t.Errorf("connection ref instance = %q, want %q", ref.GetInstanceId(), inst.GetId())
	}
	if ref.GetConnectionId() == "" {
		t.Error("the connection ref carried no connection identifier for the plugin to echo back")
	}
	if ref.GetTenantId() != metadb.DefaultTenantID {
		t.Errorf("connection ref tenant = %q", ref.GetTenantId())
	}
}

func TestTLSMaterialIsReassembledFromBothStores(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	environmentID := h.environment(t, "production")

	inst, err := h.svc.CreateInstance(ctx, CreateInstanceInput{
		EnvironmentID: environmentID,
		Name:          "tls-1",
		EngineType:    engine,
		Host:          "db.example.internal",
		Port:          5432,
		Connection: &fwv1.ConnectionSpec{
			Username: "fleetward",
			Password: storedPassword,
			Tls: &fwv1.TLSSettings{
				Enabled:       true,
				ServerName:    "db.example.internal",
				CaPem:         []byte("ca-material"),
				ClientCertPem: []byte("cert-material"),
				ClientKeyPem:  []byte("key-material"),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	// The private key belongs with the password, not in a table anyone with read access can select.
	// Byte fields are base64 in JSON, so the needles have to be encoded the same way — searching for
	// the raw text would make the negative assertion pass for the wrong reason.
	var optionsText string
	if err := h.pool.QueryRow(ctx,
		`SELECT options::text FROM connections WHERE instance_id = $1`, inst.GetId()).
		Scan(&optionsText); err != nil {
		t.Fatalf("read options: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString([]byte("key-material"))
	encodedCA := base64.StdEncoding.EncodeToString([]byte("ca-material"))
	if strings.Contains(optionsText, encodedKey) {
		t.Errorf("the client private key was stored in the connections table: %s", optionsText)
	}
	if !strings.Contains(optionsText, encodedCA) {
		t.Errorf("the CA certificate was not stored with the connection: %s", optionsText)
	}

	if _, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{InstanceId: inst.GetId()}); err != nil {
		t.Fatalf("TestConnection: %v", err)
	}

	tls := h.plugin.lastCredentials().GetTls()
	if !tls.GetEnabled() {
		t.Fatal("TLS was not enabled on the resolved credentials")
	}
	if string(tls.GetCaPem()) != "ca-material" ||
		string(tls.GetClientCertPem()) != "cert-material" ||
		string(tls.GetClientKeyPem()) != "key-material" {
		t.Error("TLS material was not reassembled from both stores")
	}
	if tls.GetServerName() != "db.example.internal" {
		t.Errorf("server_name = %q", tls.GetServerName())
	}
}

// -----------------------------------------------------------------------------------------------
// Environments and instances
// -----------------------------------------------------------------------------------------------

func TestCreateInstanceDoesNotProbe(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	// An unreachable server is exactly the kind a user most needs in their inventory, so adding one
	// must never depend on reaching it.
	if h.plugin.calls() != 0 {
		t.Errorf("CreateInstance called the plugin %d times; it must store first and probe later",
			h.plugin.calls())
	}
	if inst.GetHealth() != fwv1.HealthState_HEALTH_STATE_UNKNOWN {
		t.Errorf("health = %v, want UNKNOWN before the first probe", inst.GetHealth())
	}
	if inst.GetLastSeenAt() != nil {
		t.Error("last_seen_at was set for an instance that has never been reached")
	}
}

func TestDuplicateNamesAreRejectedPerEnvironment(t *testing.T) {
	h := newHarness(t)
	production := h.environment(t, "production")
	staging := h.environment(t, "staging")

	h.instance(t, production, "prod-1")

	if _, err := h.svc.CreateInstance(testTenantCtx(), CreateInstanceInput{
		EnvironmentID: production,
		Name:          "prod-1",
		EngineType:    engine,
		Host:          "db.example.internal",
		Port:          5432,
		Connection:    &fwv1.ConnectionSpec{Username: "fleetward", Password: storedPassword},
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("error = %v, want ErrAlreadyExists", err)
	}

	// The uniqueness scope is (tenant, environment, name), so the same name elsewhere is fine.
	h.instance(t, staging, "prod-1")
}

func TestCreateInstanceRejectsAnUnknownEnvironment(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.CreateInstance(testTenantCtx(), CreateInstanceInput{
		EnvironmentID: "0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f",
		Name:          "prod-1",
		EngineType:    engine,
		Host:          "db.example.internal",
		Port:          5432,
		Connection:    &fwv1.ConnectionSpec{Username: "fleetward", Password: storedPassword},
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}

	// A failed create must leave nothing behind — least of all a credential.
	var secretCount int
	if err := h.pool.QueryRow(testTenantCtx(), `SELECT count(*)::int FROM secrets`).Scan(&secretCount); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if secretCount != 0 {
		t.Errorf("secrets rows = %d after a failed create, want 0", secretCount)
	}
}

func TestDuplicateEnvironmentNameIsRejected(t *testing.T) {
	h := newHarness(t)
	h.environment(t, "production")

	if _, err := h.svc.CreateEnvironment(testTenantCtx(),
		CreateEnvironmentInput{Name: "production"}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("error = %v, want ErrAlreadyExists", err)
	}
}

// -----------------------------------------------------------------------------------------------
// Health and discovery
// -----------------------------------------------------------------------------------------------

func TestTestConnectionRecordsHealth(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	h.plugin.health = &fwv1.HealthStatus{
		State:         fwv1.HealthState_HEALTH_STATE_UP,
		EngineVersion: "16.2",
		Latency:       durationpb.New(3 * time.Millisecond),
		Signals: []*fwv1.HealthSignal{
			{Name: "connection_usage", Severity: fwv1.Severity_SEVERITY_INFO, Value: 4, Unit: "%"},
		},
	}

	resp, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{InstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !resp.GetSuccess() {
		t.Error("success = false for an instance the plugin reported UP")
	}
	if len(resp.GetHealth().GetSignals()) != 1 {
		t.Error("health signals were dropped on the way back")
	}

	// Caching the probe is what lets a fifty-server listing render without fifty probes.
	got, err := h.svc.GetInstance(ctx, inst.GetId())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.GetInstance().GetHealth() != fwv1.HealthState_HEALTH_STATE_UP {
		t.Errorf("stored health = %v, want UP", got.GetInstance().GetHealth())
	}
	if got.GetInstance().GetEngineVersion() != "16.2" {
		t.Errorf("stored engine_version = %q, want 16.2", got.GetInstance().GetEngineVersion())
	}
	if got.GetInstance().GetLastSeenAt() == nil {
		t.Error("last_seen_at was not recorded after a successful probe")
	}
}

// A DOWN probe is a valid answer, not a failure, and it must not move last_seen_at: that field
// means "the last time we actually talked to it".
func TestDownProbeIsRecordedWithoutMovingLastSeen(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	h.plugin.health = &fwv1.HealthStatus{State: fwv1.HealthState_HEALTH_STATE_UP, EngineVersion: "16.2"}
	if _, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{InstanceId: inst.GetId()}); err != nil {
		t.Fatalf("first TestConnection: %v", err)
	}
	first, err := h.svc.GetInstance(ctx, inst.GetId())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	seenWhenUp := first.GetInstance().GetLastSeenAt().AsTime()

	h.plugin.health = &fwv1.HealthStatus{
		State:   fwv1.HealthState_HEALTH_STATE_DOWN,
		Message: "cannot reach db.example.internal:5432",
		Error: &fwv1.PluginError{
			Code:      fwv1.ErrorCode_ERROR_CODE_CONNECTION_FAILED,
			Message:   "cannot reach db.example.internal:5432",
			Retryable: true,
		},
	}
	resp, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{InstanceId: inst.GetId()})
	if err != nil {
		t.Fatalf("a DOWN instance must not produce an error: %v", err)
	}
	if resp.GetSuccess() {
		t.Error("success = true for an instance the plugin reported DOWN")
	}

	after, err := h.svc.GetInstance(ctx, inst.GetId())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if after.GetInstance().GetHealth() != fwv1.HealthState_HEALTH_STATE_DOWN {
		t.Errorf("stored health = %v, want DOWN", after.GetInstance().GetHealth())
	}
	if !after.GetInstance().GetLastSeenAt().AsTime().Equal(seenWhenUp) {
		t.Error("a DOWN probe moved last_seen_at")
	}
}

// The wizard's case: check credentials that have not been stored, and store nothing.
func TestTestConnectionWithAnUnsavedConnection(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	h.plugin.health = &fwv1.HealthStatus{State: fwv1.HealthState_HEALTH_STATE_UP}
	resp, err := h.svc.TestConnection(ctx, &fwv1.TestConnectionRequest{
		EngineType: engine,
		Host:       "new.example.internal",
		Port:       6432,
		Connection: &fwv1.ConnectionSpec{Username: "candidate", Password: "unsaved"},
	})
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if !resp.GetSuccess() {
		t.Error("success = false")
	}
	if got := h.plugin.lastCredentials(); got.GetUsername() != "candidate" || got.GetPort() != 6432 {
		t.Errorf("the supplied connection was not used: %v", got.GetUsername())
	}

	for _, table := range []string{"instances", "connections", "secrets"} {
		var count int
		if err := h.pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows; testing a connection must persist nothing", table, count)
		}
	}
}

// The REST route for TestConnection carries instance_id in its path, so the wizard cannot leave it
// out while it is still deciding whether to add the instance at all. An identifier that resolves to
// nothing must therefore mean "not stored yet" rather than a bad request, provided the request
// describes its target completely.
func TestTestConnectionToleratesAPlaceholderInstanceID(t *testing.T) {
	h := newHarness(t)

	for _, placeholder := range []string{
		"00000000-0000-0000-0000-000000000000",
		"-",
	} {
		resp, err := h.svc.TestConnection(testTenantCtx(), &fwv1.TestConnectionRequest{
			InstanceId: placeholder,
			EngineType: engine,
			Host:       "new.example.internal",
			Port:       5432,
			Connection: &fwv1.ConnectionSpec{Username: "candidate", Password: "unsaved"},
		})
		if err != nil {
			t.Fatalf("TestConnection with instance_id %q: %v", placeholder, err)
		}
		if !resp.GetSuccess() {
			t.Errorf("success = false for instance_id %q", placeholder)
		}
	}

	// A request that names nothing usable is still a bad request.
	if _, err := h.svc.TestConnection(testTenantCtx(), &fwv1.TestConnectionRequest{
		InstanceId: "00000000-0000-0000-0000-000000000000",
		Connection: &fwv1.ConnectionSpec{Username: "candidate"},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("error = %v, want ErrInvalidArgument", err)
	}
}

func TestDiscoverInstanceCachesItsResult(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	h.plugin.discovery = &fwv1.DiscoverResponse{
		Server: &fwv1.ServerInfo{
			EngineType:    engine,
			Version:       "16.2",
			VersionString: "PostgreSQL 16.2 on aarch64",
			Attributes:    map[string]string{"max_connections": "100"},
		},
		Databases: []*fwv1.DatabaseInfo{
			{Name: "app", SizeBytes: 4096, Owner: "fleetward"},
			{Name: "postgres", IsSystem: true},
		},
		Topology: &fwv1.Topology{
			IsStandalone: true,
			Nodes: []*fwv1.Node{
				{Host: "db.example.internal", Port: 5432, Role: fwv1.NodeRole_NODE_ROLE_PRIMARY, IsSelf: true},
			},
		},
	}

	if _, err := h.svc.DiscoverInstance(ctx, inst.GetId()); err != nil {
		t.Fatalf("DiscoverInstance: %v", err)
	}

	// GetInstance answers from the cache, so opening a detail page does not query the monitored
	// database.
	before := h.plugin.calls()
	got, err := h.svc.GetInstance(ctx, inst.GetId())
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if h.plugin.calls() != before {
		t.Error("GetInstance contacted the plugin; the discovery cache exists to prevent that")
	}
	if got.GetServer().GetVersionString() != "PostgreSQL 16.2 on aarch64" {
		t.Errorf("cached server info = %q", got.GetServer().GetVersionString())
	}
	if len(got.GetDatabases()) != 2 {
		t.Errorf("cached databases = %d, want 2", len(got.GetDatabases()))
	}
	if !got.GetTopology().GetIsStandalone() {
		t.Error("cached topology lost is_standalone")
	}
	if got.GetInstance().GetEngineVersion() != "16.2" {
		t.Errorf("engine_version = %q, want 16.2", got.GetInstance().GetEngineVersion())
	}
	// Capabilities come from the plugin, which is how a capability-adaptive detail view stays
	// engine-agnostic.
	if got.GetCapabilities().GetEngineType() != engine {
		t.Errorf("capabilities = %v", got.GetCapabilities())
	}
}

func TestPluginFailureIsClassified(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	h.plugin.err = fmt.Errorf("discover failed")
	_, err := h.svc.DiscoverInstance(testTenantCtx(), inst.GetId())
	if !errors.Is(err, ErrPluginFailed) {
		t.Errorf("error = %v, want ErrPluginFailed", err)
	}
}

// -----------------------------------------------------------------------------------------------
// Listing
// -----------------------------------------------------------------------------------------------

func TestListInstancesFiltersAndPaginates(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	production := h.environment(t, "production")
	staging := h.environment(t, "staging")

	for i := range 5 {
		h.instance(t, production, fmt.Sprintf("prod-%d", i))
	}
	h.instance(t, staging, "stage-0")

	all, page, err := h.svc.ListInstances(ctx, ListInstancesFilter{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(all) != 6 {
		t.Errorf("instances = %d, want 6", len(all))
	}
	if page.TotalSize != 6 {
		t.Errorf("total_size = %d, want 6", page.TotalSize)
	}

	inProduction, _, err := h.svc.ListInstances(ctx, ListInstancesFilter{EnvironmentID: production})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(inProduction) != 5 {
		t.Errorf("instances in production = %d, want 5", len(inProduction))
	}

	// Keyset pagination must visit every row exactly once.
	seen := make(map[string]bool)
	token := ""
	for pages := 0; pages < 10; pages++ {
		batch, next, err := h.svc.ListInstances(ctx, ListInstancesFilter{PageSize: 2, PageToken: token})
		if err != nil {
			t.Fatalf("ListInstances page %d: %v", pages, err)
		}
		for _, inst := range batch {
			if seen[inst.GetId()] {
				t.Errorf("instance %s was returned twice", inst.GetName())
			}
			seen[inst.GetId()] = true
		}
		if next.NextPageToken == "" {
			break
		}
		token = next.NextPageToken
	}
	if len(seen) != 6 {
		t.Errorf("pagination visited %d of 6 instances", len(seen))
	}
}

func TestListInstancesRejectsAMalformedFilter(t *testing.T) {
	h := newHarness(t)

	// A typo in a query parameter is the caller's mistake and must not surface as a 500 from a
	// failed UUID cast.
	if _, _, err := h.svc.ListInstances(testTenantCtx(),
		ListInstancesFilter{EnvironmentID: "production"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("error = %v, want ErrInvalidArgument", err)
	}
}

// Every query filters on tenant_id. Checking it now is far cheaper than auditing every query in the
// project once a second tenant exists.
func TestListingIsScopedToTheTenant(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	h.instance(t, h.environment(t, "production"), "prod-1")

	const otherTenant = "00000000-0000-0000-0000-0000000000ff"
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO tenants (id, name, slug) VALUES ($1, 'Other', 'other')`, otherTenant); err != nil {
		t.Fatalf("seed second tenant: %v", err)
	}
	var otherEnv string
	if err := h.pool.QueryRow(ctx,
		`INSERT INTO environments (tenant_id, name) VALUES ($1, 'production') RETURNING id`,
		otherTenant).Scan(&otherEnv); err != nil {
		t.Fatalf("seed second tenant environment: %v", err)
	}
	var otherInstance string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, 'their-1', $3, 'their.example.internal', 5432)
		RETURNING id`, otherTenant, otherEnv, engine).Scan(&otherInstance); err != nil {
		t.Fatalf("seed second tenant instance: %v", err)
	}

	instances, page, err := h.svc.ListInstances(ctx, ListInstancesFilter{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != 1 || instances[0].GetName() != "prod-1" {
		t.Errorf("another tenant's instances are visible: %v", instances)
	}
	if page.TotalSize != 1 {
		t.Errorf("total_size = %d, want 1; the count is not tenant-scoped", page.TotalSize)
	}

	// Nor may another tenant's row be reachable by identifier.
	if _, err := h.svc.GetInstance(ctx, otherInstance); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetInstance across tenants error = %v, want ErrNotFound", err)
	}
	if err := h.svc.DeleteInstance(ctx, otherInstance, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteInstance across tenants error = %v, want ErrNotFound", err)
	}
}

func TestGetInstanceRejectsAMalformedIdentifier(t *testing.T) {
	h := newHarness(t)

	if _, err := h.svc.GetInstance(testTenantCtx(), "not-a-uuid"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("error = %v, want ErrInvalidArgument", err)
	}
	if _, err := h.svc.GetInstance(testTenantCtx(),
		"0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// -----------------------------------------------------------------------------------------------
// Deletion
// -----------------------------------------------------------------------------------------------

// The secrets table has no foreign key to connections — deliberately, so the secret store stays
// independent of the schema around it. Nothing but this code path will ever clean it up.
func TestDeleteInstanceRemovesItsCredentials(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()
	inst := h.instance(t, h.environment(t, "production"), "prod-1")

	var secretName string
	if err := h.pool.QueryRow(ctx,
		`SELECT secret_name FROM connections WHERE instance_id = $1`, inst.GetId()).
		Scan(&secretName); err != nil {
		t.Fatalf("read secret name: %v", err)
	}

	if err := h.svc.DeleteInstance(ctx, inst.GetId(), false); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}

	for table, expected := range map[string]int{"instances": 0, "connections": 0, "secrets": 0} {
		var count int
		if err := h.pool.QueryRow(ctx, "SELECT count(*)::int FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != expected {
			t.Errorf("%s has %d rows after deletion, want %d", table, count, expected)
		}
	}

	if _, err := h.secret.Get(ctx, secrets.Ref{TenantID: metadb.DefaultTenantID, Name: secretName}); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("the credential outlived its instance: %v", err)
	}

	// The environment survives: instances.environment_id is ON DELETE RESTRICT, and removing one
	// server must not take its environment with it.
	var environments int
	if err := h.pool.QueryRow(ctx, `SELECT count(*)::int FROM environments`).Scan(&environments); err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if environments != 1 {
		t.Errorf("environments = %d after deleting an instance, want 1", environments)
	}
}

func TestDeleteMissingInstance(t *testing.T) {
	h := newHarness(t)

	if err := h.svc.DeleteInstance(testTenantCtx(),
		"0f2d1c3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// -----------------------------------------------------------------------------------------------
// Stubs
// -----------------------------------------------------------------------------------------------

// stubRouter serves one engine type with one client.
type stubRouter struct{ client fwv1.EnginePluginClient }

func (r *stubRouter) Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error) {
	caps, err := r.Capabilities(engineType)
	if err != nil {
		return nil, nil, err
	}
	return r.client, caps, nil
}

func (r *stubRouter) Capabilities(engineType string) (*fwv1.Capabilities, error) {
	if engineType != engine {
		return nil, fmt.Errorf("no plugin available for engine type %q", engineType)
	}
	return &fwv1.Capabilities{EngineType: engine, EngineDisplayName: "Test Engine"}, nil
}

func (r *stubRouter) EngineTypes() []string { return []string{engine} }

// stubEngine records what core handed it and returns canned answers.
type stubEngine struct {
	health    *fwv1.HealthStatus
	discovery *fwv1.DiscoverResponse
	err       error

	mu        sync.Mutex
	callCount int
	lastCreds *fwv1.Credentials
	lastRef   *fwv1.ConnectionRef
}

func (s *stubEngine) record(ref *fwv1.ConnectionRef, creds *fwv1.Credentials) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callCount++
	s.lastCreds = creds
	s.lastRef = ref
}

func (s *stubEngine) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

func (s *stubEngine) lastCredentials() *fwv1.Credentials {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCreds
}

func (s *stubEngine) lastConnectionRef() *fwv1.ConnectionRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastRef
}

func (s *stubEngine) HealthCheck(_ context.Context, in *fwv1.HealthCheckRequest, _ ...grpc.CallOption) (*fwv1.HealthStatus, error) {
	s.record(in.GetConnection(), in.GetCredentials())
	if s.err != nil {
		return nil, s.err
	}
	if s.health != nil {
		return s.health, nil
	}
	return &fwv1.HealthStatus{State: fwv1.HealthState_HEALTH_STATE_UP}, nil
}

func (s *stubEngine) Discover(_ context.Context, in *fwv1.DiscoverRequest, _ ...grpc.CallOption) (*fwv1.DiscoverResponse, error) {
	s.record(in.GetConnection(), in.GetCredentials())
	if s.err != nil {
		return nil, s.err
	}
	if s.discovery != nil {
		return s.discovery, nil
	}
	return &fwv1.DiscoverResponse{Server: &fwv1.ServerInfo{EngineType: engine}}, nil
}

func (s *stubEngine) GetCapabilities(context.Context, *fwv1.GetCapabilitiesRequest, ...grpc.CallOption) (*fwv1.Capabilities, error) {
	return &fwv1.Capabilities{EngineType: engine}, nil
}

// The remaining RPCs belong to later slices. They are present because the generated client interface
// requires them, and they fail loudly so a premature caller finds out immediately.
func (s *stubEngine) GetConfig(context.Context, *fwv1.GetConfigRequest, ...grpc.CallOption) (*fwv1.GetConfigResponse, error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) CollectMetrics(context.Context, *fwv1.CollectMetricsRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[fwv1.MetricBatch], error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) Backup(context.Context, *fwv1.BackupRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[fwv1.BackupProgress], error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) Restore(context.Context, *fwv1.RestoreRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[fwv1.RestoreProgress], error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) VerifyRestore(context.Context, *fwv1.VerifyRestoreRequest, ...grpc.CallOption) (*fwv1.VerifyRestoreResult, error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) ListPITRTargets(context.Context, *fwv1.ListPITRTargetsRequest, ...grpc.CallOption) (*fwv1.PITRWindow, error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) ListPrincipals(context.Context, *fwv1.ListPrincipalsRequest, ...grpc.CallOption) (*fwv1.ListPrincipalsResponse, error) {
	return nil, errNotInThisSlice
}

func (s *stubEngine) ListBackupHistory(context.Context, *fwv1.ListBackupHistoryRequest, ...grpc.CallOption) (*fwv1.ListBackupHistoryResponse, error) {
	return nil, errNotInThisSlice
}

var errNotInThisSlice = errors.New("not implemented in slice A2")

var _ fwv1.EnginePluginClient = (*stubEngine)(nil)
