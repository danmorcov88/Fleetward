//go:build conformance

// Package conformance holds the shared plugin conformance suite (ADR-0012).
//
// One suite runs against every plugin. It reads each plugin's capability matrix and exercises
// exactly what that plugin claims to support, skipping the rest — so it is useful from a plugin's
// first commit, and it grows in coverage automatically as capabilities are turned on.
//
// Passing this suite is the merge gate for any plugin change. It is also what lets a reviewer
// trust a plugin for an engine they have never operated: "does it pass conformance?" replaces
// reading a thousand lines of engine-specific code and hoping.
//
// Scope by stage:
//
//   - Stage 0: capabilities and health. Every plugin binary must launch, handshake, declare a
//     coherent capability matrix, and refuse unimplemented RPCs cleanly.
//   - Stage 1 (now): backup → restore-to-sandbox → verify against a real engine, and the four ways
//     that path is allowed to fail. A verification system that has only ever been shown to pass is
//     indistinguishable from one that always passes, so the negative cases are the point.
//   - Later: discover, metrics, and principals against a real engine.
//
// Run with: make conformance
package conformance

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/sandbox"
	"github.com/danmorcov88/fleetward/internal/plugin/manager"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
	"github.com/danmorcov88/fleetward/internal/version"
)

// pluginDir is where `make build-plugins` puts the binaries.
const pluginDir = "../../bin/plugins"

// harness owns a manager with every built plugin loaded.
type harness struct {
	mgr     *manager.Manager
	engines []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	dir, err := filepath.Abs(pluginDir)
	if err != nil {
		t.Fatalf("resolve plugin dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("plugin directory %s not found; run `make build-plugins` first", dir)
	}

	mgr := manager.New(config.PluginsConfig{
		Dir:               dir,
		HandshakeTimeout:  60 * time.Second,
		RestartBackoffMin: time.Second,
		RestartBackoffMax: 10 * time.Second,
		HealthInterval:    30 * time.Second,
	}, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("start plugin manager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	engines := mgr.EngineTypes()
	if len(engines) == 0 {
		t.Fatalf("no plugins found in %s; run `make build-plugins` first", dir)
	}

	return &harness{mgr: mgr, engines: engines}
}

// forEachPlugin runs fn as a subtest per plugin. Every conformance test uses it, so adding an
// engine automatically adds it to every existing assertion.
func (h *harness) forEachPlugin(t *testing.T, fn func(t *testing.T, engineType string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities)) {
	t.Helper()
	for _, engineType := range h.engines {
		t.Run(engineType, func(t *testing.T) {
			client, caps, err := h.mgr.Client(engineType)
			if err != nil {
				t.Fatalf("plugin %s is not ready: %v", engineType, err)
			}
			fn(t, engineType, client, caps)
		})
	}
}

// TestPluginLaunches asserts every plugin binary starts, handshakes, and reaches ready state.
func TestPluginLaunches(t *testing.T) {
	h := newHarness(t)

	for _, info := range h.mgr.List() {
		t.Run(info.EngineType, func(t *testing.T) {
			if info.State != manager.StateReady {
				t.Fatalf("state = %s, want ready: %s", info.State, info.Message)
			}
			if len(info.MissingTools) > 0 {
				// Not fatal: the tools may legitimately be absent on a CI runner. It is reported
				// because a plugin whose tooling is missing will fail its first real backup.
				t.Logf("missing declared tools: %v", info.MissingTools)
			}
		})
	}
}

// TestCapabilitiesAreCoherent asserts each plugin's matrix is internally consistent and correctly
// identifies itself. Core trusts this matrix when deciding what to do to a production database.
func TestCapabilitiesAreCoherent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, engineType string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		caps, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}

		if err := sdk.ValidateCapabilities(caps); err != nil {
			t.Errorf("capabilities are not coherent: %v", err)
		}
		if caps.GetEngineType() != engineType {
			t.Errorf("engine_type = %q, want %q (must match the binary name)", caps.GetEngineType(), engineType)
		}
		if caps.GetContractVersion() != version.ContractVersion {
			t.Errorf("contract_version = %q, want %q", caps.GetContractVersion(), version.ContractVersion)
		}
		if caps.GetEngineDisplayName() == "" {
			t.Error("engine_display_name is empty; the UI has nothing to show")
		}
	})
}

// TestBackupCapabilityIsUsable asserts that a plugin claiming it can back up has actually published
// what core needs in order to do so.
//
// Core reads this matrix and nothing else when deciding how to back up a production database, so a
// method with no identifier, no kind, or no declared tooling is a promise core cannot act on. The
// check runs against every plugin and skips the ones that make no such claim, which is how the
// suite grows in coverage as capabilities are turned on.
func TestBackupCapabilityIsUsable(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, engineType string, _ fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		if !caps.GetSupportsOnlineBackup() && len(caps.GetBackupMethods()) == 0 {
			t.Skipf("%s declares no backup capability yet", engineType)
		}

		method := sdk.DefaultBackupMethod(caps)
		if method == nil {
			t.Fatal("a backup capability is declared but no default method is offered")
		}
		if method.GetDisplayName() == "" {
			t.Errorf("method %q has no display name; the UI has nothing to show", method.GetId())
		}

		// A method that orchestrates a native tool must say which one, so core can report a host
		// missing it as a plugin health problem rather than letting a scheduled backup fail at 3am.
		declared := append(append([]string{}, caps.GetRequiredTools()...), method.GetRequiredTools()...)
		if len(declared) == 0 {
			t.Logf("method %q declares no required tools; that is only correct if it shells out to nothing",
				method.GetId())
		}
		for _, tool := range declared {
			if strings.TrimSpace(tool) == "" {
				t.Error("a required tool is declared as an empty string")
			}
		}

		// Verification is what the product exists for. A plugin that can back up but not restore
		// into a sandbox is reported as unverifiable, which is a legitimate intermediate state —
		// but claiming checks it cannot run would report verification as failing instead.
		if len(caps.GetSupportedVerificationChecks()) > 0 && !caps.GetSupportsSandboxRestore() {
			t.Error("verification checks are declared without sandbox restore")
		}
	})
}

// TestGetCapabilitiesNeedsNoConnection asserts the capability matrix is available without a
// database. Core calls it at plugin startup, before any instance has been configured.
func TestGetCapabilitiesNeedsNoConnection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		// The request carries no ConnectionRef and no Credentials by construction.
		if _, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{}); err != nil {
			t.Fatalf("GetCapabilities must not require a connection: %v", err)
		}
	})
}

// TestCapabilitiesAreStable asserts repeated calls agree. Core caches the matrix for the plugin's
// process lifetime, so a plugin that varies its answer would leave core acting on a stale promise.
func TestCapabilitiesAreStable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, _ *fwv1.Capabilities) {
		first, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}
		second, err := client.GetCapabilities(ctx, &fwv1.GetCapabilitiesRequest{})
		if err != nil {
			t.Fatalf("GetCapabilities (second call): %v", err)
		}

		if first.GetEngineType() != second.GetEngineType() ||
			first.GetSupportsPitr() != second.GetSupportsPitr() ||
			first.GetSupportsSandboxRestore() != second.GetSupportsSandboxRestore() ||
			len(first.GetBackupMethods()) != len(second.GetBackupMethods()) {
			t.Error("capabilities changed between calls; core caches them for the process lifetime")
		}
	})
}

// TestUnsupportedRPCsAreRefusedCleanly asserts that an RPC a plugin has not implemented returns a
// typed refusal rather than hanging, panicking, or returning a nil response with a nil error.
//
// Core relies on this distinction to tell "this engine cannot do PITR" apart from "PITR is broken",
// and the UI shows the operator very different things for each.
func TestUnsupportedRPCsAreRefusedCleanly(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if len(caps.GetBackupMethods()) > 0 {
			t.Skip("plugin declares backup methods; the full contract is exercised by the Stage 1 suite")
		}

		// A plugin with no backup methods must refuse Backup rather than accept it.
		stream, err := client.Backup(ctx, &fwv1.BackupRequest{BackupId: "conformance-probe"})
		if err != nil {
			assertTypedRefusal(t, err)
			return
		}
		if _, err = stream.Recv(); err == nil {
			t.Fatal("a plugin with no backup methods accepted a Backup request")
		}
		assertTypedRefusal(t, err)
	})
}

// TestPITRWindowIsAnAnswerNotAnError asserts that a plugin without point-in-time recovery reports
// an unavailable window with a reason, rather than failing the call.
//
// The UI can explain "WAL archiving is disabled"; it cannot do anything useful with a stack trace.
func TestPITRWindowIsAnAnswerNotAnError(t *testing.T) {
	h := newHarness(t)

	h.forEachPlugin(t, func(t *testing.T, _ string, client fwv1.EnginePluginClient, caps *fwv1.Capabilities) {
		if caps.GetSupportsPitr() {
			t.Skip("plugin supports PITR; covered by the Stage 1 suite against a real instance")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		window, err := client.ListPITRTargets(ctx, &fwv1.ListPITRTargetsRequest{})
		if err != nil {
			t.Fatalf("ListPITRTargets must answer rather than fail when PITR is unsupported: %v", err)
		}
		if window.GetAvailable() {
			t.Error("window reports available = true but capabilities say supports_pitr = false")
		}
		if window.GetUnavailableReason() == "" {
			t.Error("unavailable_reason is empty; the UI has nothing to explain to the operator")
		}
	})
}

// assertTypedRefusal checks that an error carries a structured PluginError with a code core can act
// on, rather than an opaque string it would have to parse.
func assertTypedRefusal(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("plugin hung instead of refusing: %v", err)
	}

	pe, ok := sdk.PluginErrorFrom(err)
	if !ok {
		t.Errorf("error carries no structured PluginError, so core cannot classify it: %v", err)
		return
	}
	if pe.GetCode() == fwv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Errorf("PluginError has no code: %v", pe.GetMessage())
	}
}

// -------------------------------------------------------------------------------------------
// Stage 1 harness: a real engine, a real object store, and real throwaway containers
// -------------------------------------------------------------------------------------------
//
// Everything below exists so that the backup → restore → verify path can be driven exactly the way
// the control plane drives it: core begins a multipart upload and hands the plugin presigned part
// grants, core provisions a container from the plugin's own SandboxTemplate, core hands back the
// backup's metadata verbatim. Nothing here knows an engine's name.
//
// The one thing the contract deliberately cannot express is how to put rows into a database —
// Fleetward never writes to a monitored instance (CLAUDE.md §1), so there is no RPC that could.
// That gap is filled by a per-engine Fixture; see fixtures_test.go.

const (
	minioImage  = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	minioUser   = "fleetward"
	minioSecret = "fleetward-conformance"
	minioBucket = "fleetward-conformance"

	// uploadParts is what core grants per artifact. Mirrored rather than reduced, so a plugin that
	// mishandles the grant list fails here for the same reason it would fail in production.
	uploadParts = 64

	// sandboxLabelPrefix is deliberately the default one, so that the leak check below is literally
	// the acceptance command from the slice brief: `docker ps -a --filter label=fleetward.sandbox`.
	// A developer running this suite while a control plane holds live sandboxes on the same daemon
	// will see them counted; that is the same trade the sandbox integration tests make.
	sandboxLabelPrefix = "fleetward"
	sandboxMarkerLabel = sandboxLabelPrefix + ".sandbox"

	startTimeout = 5 * time.Minute
)

// Shared infrastructure. One object store and one container runtime serve the whole run: they are
// not what is under test, and starting a MinIO per case would triple the suite's runtime for no
// extra evidence. Sandboxes are never shared — a restore populates one, so each case that restores
// gets its own.
var (
	storeOnce sync.Once
	sharedS3  objstore.ObjectStore
	storeErr  error
	stopStore func()

	providerOnce sync.Once
	sharedBoxes  sandbox.Provider
	providerErr  error
)

// objectStore returns the shared S3-compatible store, starting MinIO on first use.
func objectStore(t *testing.T) objstore.ObjectStore {
	t.Helper()

	storeOnce.Do(func() { sharedS3, stopStore, storeErr = startMinIO() })
	if storeErr != nil {
		t.Fatalf("start the object store: %v", storeErr)
	}
	return sharedS3
}

func startMinIO() (objstore.ObjectStore, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			ExposedPorts: []string{"9000/tcp"},
			Cmd:          []string{"server", "/data"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     minioUser,
				"MINIO_ROOT_PASSWORD": minioSecret,
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStartupTimeout(startTimeout),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("start minio: %w", err)
	}
	stop := func() { _ = testcontainers.TerminateContainer(container) }

	host, err := container.Host(ctx)
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("minio host: %w", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("minio port: %w", err)
	}

	store, err := objstore.NewS3Store(config.ObjStoreConfig{
		Endpoint:      fmt.Sprintf("%s:%d", host, port.Num()),
		Region:        "us-east-1",
		Bucket:        minioBucket,
		AccessKey:     minioUser,
		SecretKey:     minioSecret,
		PresignTTL:    time.Hour,
		PartSizeBytes: objstore.MinPartSizeBytes,
	})
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("build the object store: %w", err)
	}
	if err := store.EnsureBucket(ctx); err != nil {
		_ = store.Close()
		stop()
		return nil, nil, fmt.Errorf("create the bucket: %w", err)
	}

	return store, func() { _ = store.Close(); stop() }, nil
}

// sandboxProvider returns the shared Docker-backed provider, or skips when there is no daemon.
func sandboxProvider(t *testing.T) sandbox.Provider {
	t.Helper()

	providerOnce.Do(func() {
		sharedBoxes, providerErr = sandbox.New(config.SandboxConfig{
			Provider:       "docker",
			StartupTimeout: 5 * time.Minute,
			MaxLifetime:    time.Hour,
			LabelPrefix:    sandboxLabelPrefix,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	if providerErr != nil {
		t.Fatalf("build the sandbox provider: %v", providerErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sharedBoxes.HealthCheck(ctx); err != nil {
		t.Skipf("no container runtime is available: %v", err)
	}
	return sharedBoxes
}

// newInstance provisions one throwaway instance of the plugin's engine and guarantees it is gone.
//
// It is used for the source database as well as for the restore target. Standing the source up the
// same way is not a shortcut: it is the only engine-agnostic way core has to run an engine at all,
// and it means the suite needs no image name, no port, and no environment of its own.
func newInstance(t *testing.T, engineType string, caps *fwv1.Capabilities, role string) *fwv1.Credentials {
	t.Helper()

	provider := sandboxProvider(t)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// No engine version: the source and the target must be the same version, and the template's
	// own default_tag is the only version the plugin has committed to.
	box, err := provider.Provision(ctx, sandbox.Spec{
		EngineType: engineType,
		Template:   caps.GetSandboxTemplate(),
		Labels:     map[string]string{"fleetward.conformance": role},
	})
	if err != nil {
		t.Fatalf("provision a %s instance for %s: %v", engineType, role, err)
	}
	t.Cleanup(func() {
		destroyCtx, destroyCancel := context.WithTimeout(context.Background(), time.Minute)
		defer destroyCancel()
		if err := box.Destroy(destroyCtx); err != nil {
			t.Errorf("destroy the %s instance: %v", role, err)
		}
	})

	return box.Credentials()
}

// requireVerifiablePlugin skips unless this plugin claims the whole path and can actually run it.
//
// Capability-gated, per ADR-0012: a plugin that does not claim sandbox restore is skipped rather
// than failed, which is what keeps the suite useful from a plugin's first commit while remaining
// the merge gate for a complete one.
func requireVerifiablePlugin(t *testing.T, h *harness, engineType string, caps *fwv1.Capabilities) Fixture {
	t.Helper()

	if len(caps.GetBackupMethods()) == 0 {
		t.Skipf("%s declares no backup method yet", engineType)
	}
	if !caps.GetSupportsSandboxRestore() {
		t.Skipf("%s does not claim sandbox restore yet", engineType)
	}
	if caps.GetSandboxTemplate().GetImageRepository() == "" {
		t.Fatal("sandbox restore is claimed but no sandbox template is published for core to use")
	}

	// A plugin orchestrates native tooling by design, so a host missing it cannot exercise this
	// path. The manager already knows: it checks the declared required_tools at launch.
	for _, info := range h.mgr.List() {
		if info.EngineType == engineType && len(info.MissingTools) > 0 {
			t.Skipf("%s needs %v on PATH to be exercised end to end", engineType, info.MissingTools)
		}
	}

	fixture, ok := fixtures[engineType]
	if !ok {
		t.Skipf("%s has no conformance fixture; see docs/dev/writing-an-engine-plugin.md §10", engineType)
	}
	return fixture
}

// -------------------------------------------------------------------------------------------
// Driving the contract the way core drives it
// -------------------------------------------------------------------------------------------

// newID mints an identifier for one request. crypto/rand rather than a new module dependency: the
// only property needed is that two cases never collide on an object key.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("conformance: no entropy: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// artifact is one completed backup: where it landed, and everything core records about it.
type artifact struct {
	key    string
	result *fwv1.BackupResult
}

// runBackup plays core's half of the upload protocol (ADR-0021) and returns the stored artifact.
func runBackup(t *testing.T, ctx context.Context, store objstore.ObjectStore, client fwv1.EnginePluginClient, caps *fwv1.Capabilities, creds *fwv1.Credentials, name string) artifact {
	t.Helper()

	backupID := newID()
	key := objstore.ArtifactKey("conformance", caps.GetEngineType(), backupID, name)

	upload, err := store.CreateMultipartUpload(ctx, key, uploadParts, time.Hour)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	completed := false
	defer func() {
		if !completed {
			_ = store.AbortMultipartUpload(context.WithoutCancel(ctx), key, upload.UploadID)
		}
	}()

	partURLs := make([]*fwv1.PresignedURL, 0, len(upload.Parts))
	for _, grant := range upload.Parts {
		partURLs = append(partURLs, &fwv1.PresignedURL{
			Url: grant.URL, Method: grant.Method, Headers: grant.Headers,
		})
	}

	stream, err := client.Backup(ctx, &fwv1.BackupRequest{
		Credentials: creds,
		BackupId:    backupID,
		MethodId:    sdk.DefaultBackupMethod(caps).GetId(),
		Target: &fwv1.ArtifactTarget{
			Object:            &fwv1.ObjectRef{Bucket: store.Bucket(), Key: key},
			PartUrls:          partURLs,
			PartSizeBytes:     upload.PartSize,
			ChecksumAlgorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
		},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var result *fwv1.BackupResult
	for {
		progress, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Backup stream: %v", err)
		}
		switch progress.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED:
			result = progress.GetResult()
		case fwv1.JobPhase_JOB_PHASE_FAILED:
			t.Fatalf("backup failed: %s", progress.GetError().GetMessage())
		}
	}
	if result == nil {
		// Core cannot interpret this either: the plugin may have finished or may have died.
		t.Fatal("the plugin ended the backup stream without a terminal message")
	}

	parts := make([]objstore.CompletedPart, 0, len(result.GetParts()))
	for _, p := range result.GetParts() {
		parts = append(parts, objstore.CompletedPart{PartNumber: int(p.GetPartNumber()), ETag: p.GetEtag()})
	}
	if _, err := store.CompleteMultipartUpload(ctx, key, upload.UploadID, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	completed = true

	if result.GetChecksum().GetValue() == "" {
		t.Fatal("the backup recorded no checksum, so no verification of it could ever be trusted")
	}
	if len(result.GetManifest().GetEntries()) == 0 {
		t.Fatal("the backup carries no manifest, so restoring it would prove nothing")
	}

	return artifact{key: key, result: result}
}

// artifactSource builds the download grant core hands a plugin when it restores.
func artifactSource(t *testing.T, ctx context.Context, store objstore.ObjectStore, a artifact) *fwv1.ArtifactSource {
	t.Helper()

	grant, err := store.PresignGet(ctx, a.key, time.Hour)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	return &fwv1.ArtifactSource{
		Object:      &fwv1.ObjectRef{Bucket: store.Bucket(), Key: a.key},
		DownloadUrl: &fwv1.PresignedURL{Url: grant.URL, Method: grant.Method, Headers: grant.Headers},
		Checksum:    a.result.GetChecksum(),
		Role:        fwv1.ArtifactRole_ARTIFACT_ROLE_BASE,
		SizeBytes:   a.result.GetSizeBytes(),
	}
}

// sandboxTarget is what core builds from a provisioned sandbox.
func sandboxTarget(creds *fwv1.Credentials) *fwv1.RestoreTarget {
	return &fwv1.RestoreTarget{
		Kind:        fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX,
		Credentials: creds,
		SandboxId:   "conformance-sandbox",
	}
}

// restore drives the Restore RPC to its terminal message, exactly as core does. A failure is
// returned rather than fatal, because half this suite is about what a failure means.
func restore(ctx context.Context, client fwv1.EnginePluginClient, a artifact, source *fwv1.ArtifactSource, target *fwv1.RestoreTarget, methodID string) (*fwv1.RestoreProgress, error) {
	stream, err := client.Restore(ctx, &fwv1.RestoreRequest{
		RestoreId: newID(),
		Artifacts: []*fwv1.ArtifactSource{source},
		Target:    target,
		MethodId:  methodID,
		// The backup's own metadata, handed straight back. Core never inspects it.
		Options: a.result.GetMetadata(),
	})
	if err != nil {
		return nil, err
	}

	var terminal *fwv1.RestoreProgress
	for {
		progress, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch progress.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED, fwv1.JobPhase_JOB_PHASE_FAILED:
			terminal = progress
		}
	}
	if terminal == nil {
		return nil, errors.New("the plugin ended the restore stream without a terminal message")
	}
	return terminal, nil
}

// restoreInto restores an artifact and fails the test unless it completed.
func restoreInto(t *testing.T, ctx context.Context, client fwv1.EnginePluginClient, store objstore.ObjectStore, a artifact, target *fwv1.RestoreTarget, methodID string) {
	t.Helper()

	terminal, err := restore(ctx, client, a, artifactSource(t, ctx, store, a), target, methodID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("restore phase = %s: %s", terminal.GetPhase(), terminal.GetError().GetMessage())
	}
}

// copyArtifact writes a mutated copy of a stored artifact under a fresh key.
//
// The mutation happens in the bucket, never through the plugin: corruption injected through the
// code path under test proves less than it appears to, because it can only ever exercise the
// branch someone remembered to write. This is what bit rot and a half-finished upload look like.
func copyArtifact(t *testing.T, ctx context.Context, store objstore.ObjectStore, a artifact, mutate func([]byte) []byte) artifact {
	t.Helper()

	reader, _, err := store.Get(ctx, a.key)
	if err != nil {
		t.Fatalf("read the stored artifact: %v", err)
	}
	original, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatalf("read the stored artifact: %v", err)
	}

	mutated := mutate(append([]byte(nil), original...))
	if bytes.Equal(mutated, original) {
		t.Fatal("the mutation changed nothing, so the case proves nothing")
	}

	key := a.key + ".corrupt-" + newID()
	if _, err := store.Put(ctx, key, bytes.NewReader(mutated), int64(len(mutated)), objstore.PutOptions{}); err != nil {
		t.Fatalf("store the corrupted artifact: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.WithoutCancel(ctx), key) })

	// The checksum and size stay exactly as they were recorded when the good artifact was written.
	// That is the whole point: the evidence chain says these bytes are not those bytes.
	return artifact{key: key, result: a.result}
}

// TestMain guarantees the suite leaves nothing behind. It runs on every change in CI, so a leaked
// container per run would degrade the runner quietly rather than break anything visibly.
func TestMain(m *testing.M) {
	code := m.Run()

	if stopStore != nil {
		stopStore()
	}
	if sharedBoxes != nil {
		_ = sharedBoxes.Close()
	}

	if leaked, err := survivingSandboxes(); err != nil {
		fmt.Fprintf(os.Stderr, "could not check for leaked sandboxes: %v\n", err)
	} else if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "%d sandbox container(s) survived the conformance run: %s\n",
			len(leaked), strings.Join(leaked, ", "))
		code = 1
	}

	os.Exit(code)
}

// survivingSandboxes is `docker ps -a --filter label=fleetward.sandbox`, expressed against the API.
func survivingSandboxes() ([]string, error) {
	cli, err := dockerclient.New(dockerclient.WithHostFromEnv(), dockerclient.WithTLSClientConfigFromEnv())
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := cli.ContainerList(ctx, dockerclient.ContainerListOptions{
		All:     true,
		Filters: make(dockerclient.Filters).Add("label", sandboxMarkerLabel),
	})
	if err != nil {
		// No daemon means every case that could have leaked was skipped.
		return nil, nil
	}

	names := make([]string, 0, len(result.Items))
	for _, summary := range result.Items {
		if len(summary.Names) > 0 {
			names = append(names, summary.Names[0])
			continue
		}
		names = append(names, summary.ID)
	}
	return names, nil
}
