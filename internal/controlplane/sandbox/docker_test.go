//go:build integration

// Integration tests for the Docker sandbox provider, run against a real daemon.
//
// The thing worth testing here is not that a container starts — it is that nothing survives. A
// leaked sandbox breaks no test; it consumes a machine slowly until someone notices. So every test
// below asserts absence, and TestMain fails the run if a single labelled container is left behind.
//
// These tests use the default label prefix on purpose, because that is what the acceptance check
// in the slice brief greps for. A developer running them while a real control plane is holding
// live sandboxes on the same daemon will have those swept: that is precisely what Sweep is for.
//
// Run with: go test -tags=integration ./internal/controlplane/sandbox/...
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

const (
	// The same image the PostgreSQL plugin's integration tests use, so a developer running the
	// whole suite pulls it once.
	testEngineVersion = "16.2"
	testEngineType    = "postgresql"
	testStartupBudget = 3 * time.Minute
)

// testTemplate is what a plugin will declare in A5. Core has no idea what POSTGRES_PASSWORD means;
// the template says where core's generated identity belongs, and that is the whole contract.
func testTemplate() *fwv1.SandboxTemplate {
	return &fwv1.SandboxTemplate{
		ImageRepository: "postgres",
		DefaultTag:      "16-alpine",
		TagTemplate:     "{{ .Major }}-alpine",
		ContainerPort:   5432,
		Env: map[string]string{
			"POSTGRES_USER":     "{{ .Username }}",
			"POSTGRES_PASSWORD": "{{ .Password }}",
			"POSTGRES_DB":       "{{ .Database }}",
		},
		// -h 127.0.0.1 matters. PostgreSQL runs a temporary socket-only server during initdb, and
		// a probe that reaches it would report ready to a server about to be shut down.
		ReadinessCommand: []string{
			"pg_isready",
			"-h", "127.0.0.1",
			"-p", "{{ .Port }}",
			"-U", "{{ .Username }}",
			"-d", "{{ .Database }}",
		},
		ReadinessTimeout: durationpb.New(2 * time.Minute),
	}
}

func testSpec() Spec {
	return Spec{
		EngineType:    testEngineType,
		EngineVersion: testEngineVersion,
		Template:      testTemplate(),
		Labels:        map[string]string{"fleetward.test": "true"},
	}
}

// newTestProvider builds a provider and guarantees it is closed.
func newTestProvider(t *testing.T) *DockerProvider {
	t.Helper()

	// Discard logs: these tests are noisy by design, and a failure is asserted rather than read.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	p, err := NewDockerProvider(sandboxConfig("docker"), log)
	if err != nil {
		t.Fatalf("new docker provider: %v", err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close provider: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.HealthCheck(ctx); err != nil {
		t.Skipf("docker is not available: %v", err)
	}

	return p
}

// requireGone fails unless the container is absent from the daemon.
func requireGone(t *testing.T, p *DockerProvider, sandboxID string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	found, err := p.list(ctx)
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	for _, summary := range found {
		if summary.Labels[p.labels.id] == sandboxID {
			t.Fatalf("sandbox %s survived as container %s (%s)", sandboxID, summary.ID, summary.State)
		}
	}
}

// TestSandboxLifecycle covers the whole happy path: it comes up, it is reachable with the
// credentials core generated, and it goes away.
func TestSandboxLifecycle(t *testing.T) {
	p := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	box, err := p.Provision(ctx, testSpec())
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	creds := box.Credentials()
	if creds.GetHost() == "" || creds.GetPort() == 0 {
		t.Fatalf("credentials have no endpoint: %+v", creds)
	}
	if creds.GetPort() == 5432 {
		t.Errorf("port %d looks fixed rather than ephemeral; concurrent verifications would collide",
			creds.GetPort())
	}
	if creds.GetPassword() == "" {
		t.Error("the sandbox was created without a generated password")
	}

	// The point of Credentials() is that a plugin's Restore can use them unchanged.
	assertConnects(t, creds)

	// A returned copy must not be the sandbox's own state.
	box.Credentials().Host = "tampered"
	if box.Credentials().GetHost() == "tampered" {
		t.Error("Credentials() handed out its own state instead of a copy")
	}

	destroyCtx, cancelDestroy := context.WithTimeout(context.Background(), destroyTimeout)
	defer cancelDestroy()

	if err := box.Destroy(destroyCtx); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	// Destroy runs from a defer and may race the reaper or a sweep, so a second call has to be
	// harmless rather than merely tolerated.
	if err := box.Destroy(destroyCtx); err != nil {
		t.Fatalf("second destroy: %v", err)
	}

	requireGone(t, p, box.ID())
}

// TestRunDestroysOnPanic is the reason Run exists. A verification that panics is exactly the case
// a documented "remember to defer Destroy" convention does not cover.
func TestRunDestroysOnPanic(t *testing.T) {
	p := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	var sandboxID string

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("the panic did not propagate out of Run")
			}
		}()

		_ = Run(ctx, p, testSpec(), func(_ context.Context, box Sandbox) error {
			sandboxID = box.ID()
			panic("verification exploded")
		})
	}()

	if sandboxID == "" {
		t.Fatal("the sandbox was never provisioned")
	}
	requireGone(t, p, sandboxID)
}

// TestRunDestroysAfterCancellation covers the trap that makes teardown fail in production: the
// context is already cancelled by the time cleanup runs.
func TestRunDestroysAfterCancellation(t *testing.T) {
	p := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	var sandboxID string

	err := Run(ctx, p, testSpec(), func(inner context.Context, box Sandbox) error {
		sandboxID = box.ID()
		// Simulate a verification that fails because its deadline passed.
		cancelled, cancelInner := context.WithCancel(inner)
		cancelInner()
		return cancelled.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	requireGone(t, p, sandboxID)
}

// TestProvisionLeavesNothingBehindWhenReadinessNeverPasses covers the failure that would otherwise
// leak the most: a container that starts, never becomes usable, and is never handed to a caller
// who could defer its teardown.
func TestProvisionLeavesNothingBehindWhenReadinessNeverPasses(t *testing.T) {
	p := newTestProvider(t)
	// Long enough to create and start, short enough not to slow the suite down.
	p.cfg.StartupTimeout = 15 * time.Second

	before := countSandboxes(t, p)

	spec := testSpec()
	spec.Template.ReadinessCommand = []string{"false"}
	spec.Template.ReadinessTimeout = durationpb.New(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	box, err := p.Provision(ctx, spec)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error = %v, want ErrNotReady", err)
	}
	if box != nil {
		t.Fatal("Provision returned a sandbox alongside an error")
	}

	if after := countSandboxes(t, p); after != before {
		t.Fatalf("sandbox containers went from %d to %d; the failed one leaked", before, after)
	}
}

// TestSweepRemovesAnOrphanButNotALiveSandbox is the defence that saves you after the control plane
// is killed mid-verification, which is when a leak actually happens.
func TestSweepRemovesAnOrphanButNotALiveSandbox(t *testing.T) {
	abandoned := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	orphan, err := abandoned.Provision(ctx, testSpec())
	if err != nil {
		t.Fatalf("provision the sandbox to abandon: %v", err)
	}
	// Deliberately not destroyed: this stands in for a process that died holding it.

	// A second provider is a second process as far as the labels are concerned.
	restarted := newTestProvider(t)

	live, err := restarted.Provision(ctx, testSpec())
	if err != nil {
		t.Fatalf("provision the live sandbox: %v", err)
	}
	t.Cleanup(func() {
		destroyCtx, cancelDestroy := context.WithTimeout(context.Background(), destroyTimeout)
		defer cancelDestroy()
		if err := live.Destroy(destroyCtx); err != nil {
			t.Errorf("destroy the live sandbox: %v", err)
		}
	})

	removed, err := restarted.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed < 1 {
		t.Fatalf("sweep removed %d containers, want at least the orphan", removed)
	}

	requireGone(t, restarted, orphan.ID())

	// A sweep that also killed the sandboxes of the running process would turn every concurrent
	// verification into a spurious failure.
	if !stillExists(t, restarted, live.ID()) {
		t.Fatal("sweep destroyed a sandbox owned by the running process")
	}
}

// TestReaperDestroysASandboxPastItsCeiling covers the case a deferred teardown cannot: a
// verification that hung rather than failed, so nothing is ever going to call Destroy.
func TestReaperDestroysASandboxPastItsCeiling(t *testing.T) {
	p := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), testStartupBudget)
	defer cancel()

	spec := testSpec()
	spec.Lifetime = time.Nanosecond

	box, err := p.Provision(ctx, spec)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	removed, err := p.reapExpired(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if removed != 1 {
		t.Fatalf("reaper removed %d sandboxes, want 1", removed)
	}

	requireGone(t, p, box.ID())

	// The sandbox handle must survive its container being reaped out from under it.
	if err := box.Destroy(ctx); err != nil {
		t.Fatalf("destroy after the reaper already removed it: %v", err)
	}
}

// TestProvisionRejectsAnEngineWithoutATemplate keeps a plugin that cannot be verified from looking
// like a plugin that failed verification.
func TestProvisionRejectsAnEngineWithoutATemplate(t *testing.T) {
	p := newTestProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := p.Provision(ctx, Spec{EngineType: testEngineType}); !errors.Is(err, ErrNoTemplate) {
		t.Fatalf("error = %v, want ErrNoTemplate", err)
	}
}

// assertConnects proves the credentials are usable, which is the only definition of "ready" that
// matters to the restore that will run here in A5.
func assertConnects(t *testing.T, creds *fwv1.Credentials) {
	t.Helper()

	// Built field by field rather than as a DSN, for the same reason the PostgreSQL plugin does:
	// a connection string containing a password ends up in error messages and stack traces.
	cfg, err := pgx.ParseConfig("")
	if err != nil {
		t.Fatalf("pgx config: %v", err)
	}
	cfg.Host = creds.GetHost()
	cfg.Port = uint16(creds.GetPort())
	cfg.User = creds.GetUsername()
	cfg.Password = creds.GetPassword()
	cfg.Database = creds.GetDatabase()

	connectCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.ConnectConfig(connectCtx, cfg)
	if err != nil {
		t.Fatalf("connect to the sandbox: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(connectCtx)) }()

	var one int
	if err := conn.QueryRow(connectCtx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query the sandbox: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d", one)
	}
}

func countSandboxes(t *testing.T, p *DockerProvider) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	found, err := p.list(ctx)
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	return len(found)
}

func stillExists(t *testing.T, p *DockerProvider, sandboxID string) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	found, err := p.list(ctx)
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	for _, summary := range found {
		if summary.Labels[p.labels.id] == sandboxID {
			return true
		}
	}
	return false
}

// TestMain turns the acceptance check from the slice brief —
//
//	docker ps -a --filter "label=fleetward.sandbox" --format '{{.Names}}'   # empty
//
// into something CI enforces. A suite that passes while leaving containers behind has tested the
// wrong thing.
func TestMain(m *testing.M) {
	code := m.Run()

	if leaked, err := survivingSandboxes(); err != nil {
		fmt.Fprintf(os.Stderr, "could not check for leaked sandboxes: %v\n", err)
	} else if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "%d sandbox container(s) survived the test run: %s\n",
			len(leaked), strings.Join(leaked, ", "))
		code = 1
	}

	os.Exit(code)
}

// survivingSandboxes lists every container still carrying the sandbox marker label.
func survivingSandboxes() ([]string, error) {
	p, err := NewDockerProvider(sandboxConfig("docker"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = p.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := p.HealthCheck(ctx); err != nil {
		// Docker was unavailable, so every test skipped and there is nothing to have leaked.
		return nil, nil
	}

	found, err := p.list(ctx)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(found))
	for _, summary := range found {
		if len(summary.Names) > 0 {
			names = append(names, summary.Names[0])
			continue
		}
		names = append(names, summary.ID)
	}
	return names, nil
}
