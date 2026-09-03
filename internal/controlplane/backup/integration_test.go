//go:build integration

// Integration tests for the backup service against a real metadata store and a real object store.
//
// What is under test is the half of the loop core owns: mint a scoped upload grant, drive the
// plugin's stream, assemble the object, and write down what happened. The plugin is a stub on
// purpose — the PostgreSQL plugin is covered against a real server and a real MinIO by its own
// integration tests, and core's tests staying engine-agnostic is the architectural point rather
// than an omission.
//
// Requires Docker and nothing else.
//
// Run with: go test -tags=integration ./internal/controlplane/backup/...
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
)

const (
	metaImage    = "postgres:16-alpine"
	minioImage   = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	startTimeout = 3 * time.Minute
	engine       = "testengine"
	testBucket   = "fleetward-backups"
	// testPartSize is the smallest S3 permits for a non-final part. Using the floor rather than the
	// production default keeps the payload that spans several parts small enough to be quick.
	testPartSize = objstore.MinPartSizeBytes
)

// harness is one test's isolated world.
type harness struct {
	svc       *Service
	pool      *pgxpool.Pool
	store     objstore.ObjectStore
	plugin    *stubEngine
	router    *stubRouter
	sandboxes *stubSandboxes
	resolver  *stubResolver
	log       *slog.Logger
	instance  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(testTenantCtx(), startTimeout)
	defer cancel()

	pool := startMetaDB(t, ctx)
	store := startMinIO(t, ctx)
	plugin := &stubEngine{}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	instanceID := seedInstance(t, ctx, pool)
	// The container runtime is stubbed while the metadata store and the object store are real. What
	// is under test here is core's own orchestration and bookkeeping; that a Docker container really
	// starts and is really destroyed is proven by internal/controlplane/sandbox's own integration
	// tests, and duplicating it would only make this suite slower and flakier.
	sandboxes := &stubSandboxes{}

	router := &stubRouter{client: plugin}
	// The retention policy every test starts with: enabled, and with the floor and the ceiling at
	// their production defaults. A test that needs different limits rebuilds the service through
	// withRetention rather than reaching into the field, so what it changed is visible at the call
	// site.
	svc := New(pool, store, router, &stubResolver{instanceID: instanceID}, sandboxes,
		RetentionPolicy{Enabled: true, Interval: time.Hour, MinKeep: 1, MaxPerSweep: 500}, log)
	t.Cleanup(func() { _ = svc.Close() })

	return &harness{
		svc: svc, pool: pool, store: store, plugin: plugin, router: router,
		sandboxes: sandboxes, instance: instanceID, log: log,
		resolver: &stubResolver{instanceID: instanceID},
	}
}

// withRetention returns a service over the same database and object store, under a different
// retention policy. It exists so a test that needs a floor of three, or a ceiling of one, says so
// where it can be read rather than mutating the harness underneath the other tests.
func (h *harness) withRetention(t *testing.T, policy RetentionPolicy) *Service {
	t.Helper()
	svc := New(h.pool, h.store, h.router, h.resolver, h.sandboxes, policy, h.log)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func startMetaDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.Run(ctx, metaImage,
		postgres.WithDatabase("fleetward"),
		postgres.WithUsername("fleetward"),
		postgres.WithPassword("fleetward-integration"),
		testcontainers.WithWaitStrategy(
			// initdb restarts the server once, so the log line appears twice.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startTimeout)),
	)
	if err != nil {
		t.Fatalf("start metadata postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate metadata postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := metadb.Open(ctx, config.MetaDBConfig{DSN: dsn, ConnectTimeout: 30 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db.Pool()
}

func startMinIO(t *testing.T, ctx context.Context) objstore.ObjectStore {
	t.Helper()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		Started: true,
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			ExposedPorts: []string{"9000/tcp"},
			Cmd:          []string{"server", "/data"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     "fleetward",
				"MINIO_ROOT_PASSWORD": "fleetward-integration",
			},
			WaitingFor: wait.ForHTTP("/minio/health/live").
				WithPort("9000/tcp").
				WithStartupTimeout(startTimeout),
		},
	})
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate minio: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("minio host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("minio port: %v", err)
	}

	store, err := objstore.NewS3Store(config.ObjStoreConfig{
		Endpoint:      fmt.Sprintf("%s:%d", host, port.Num()),
		Region:        "us-east-1",
		Bucket:        testBucket,
		AccessKey:     "fleetward",
		SecretKey:     "fleetward-integration",
		PresignTTL:    time.Hour,
		PartSizeBytes: testPartSize,
	})
	if err != nil {
		t.Fatalf("build object store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.EnsureBucket(ctx); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return store
}

// seedInstance inserts the rows the foreign keys on jobs and backups require.
func seedInstance(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()

	var environmentID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (tenant_id, name, is_production)
		VALUES ($1, 'production', TRUE) RETURNING id`, metadb.DefaultTenantID).Scan(&environmentID); err != nil {
		t.Fatalf("seed environment: %v", err)
	}

	var instanceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, 'prod-1', $3, 'db.example.internal', 5432) RETURNING id`,
		metadb.DefaultTenantID, environmentID, engine).Scan(&instanceID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return instanceID
}

// waitForState polls until the backup reaches a terminal state, which is how a real caller observes
// an asynchronous run.
func (h *harness) waitForState(t *testing.T, backupID string) *fwv1.Backup {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		b, _, err := h.svc.GetBackup(testTenantCtx(), backupID)
		if err != nil {
			t.Fatalf("GetBackup: %v", err)
		}
		switch b.GetState() {
		case fwv1.BackupState_BACKUP_STATE_SUCCEEDED, fwv1.BackupState_BACKUP_STATE_FAILED,
			fwv1.BackupState_BACKUP_STATE_CANCELED, fwv1.BackupState_BACKUP_STATE_EXPIRED:
			return b
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("backup %s never reached a terminal state", backupID)
	return nil
}

// TestRunBackupPersistsTheArtifactAndItsManifest is the orchestration this slice exists to deliver.
func TestRunBackupPersistsTheArtifactAndItsManifest(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	// 12 MiB over a 5 MiB part size: three parts, the last one short. Multipart assembly is the
	// riskiest new path in this slice, so the test exercises it against a real store rather than
	// settling for a payload that fits in one part.
	h.plugin.payload = []byte(strings.Repeat("fleetward-artifact-bytes ", 12<<20/25))
	h.plugin.manifest = &fwv1.SourceManifest{
		Entries: []*fwv1.ManifestEntry{
			{Database: "app", ObjectName: "public.customers", RecordCount: 40},
			{Database: "app", ObjectName: "public.orders", RecordCount: 120},
		},
		TotalObjects: 2,
		TotalRecords: 160,
	}

	backupID, jobID, err := h.svc.RunBackup(ctx, RunBackupInput{
		InstanceID:        h.instance,
		TriggeredManually: true,
	})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}

	b := h.waitForState(t, backupID)
	if b.GetState() != fwv1.BackupState_BACKUP_STATE_SUCCEEDED {
		t.Fatalf("state = %v: %s", b.GetState(), b.GetErrorMessage())
	}
	if b.GetSizeBytes() != int64(len(h.plugin.payload)) {
		t.Errorf("size = %d, want %d", b.GetSizeBytes(), len(h.plugin.payload))
	}
	if b.GetChecksum().GetValue() == "" {
		t.Error("no checksum was recorded")
	}
	if !b.GetTriggeredManually() {
		t.Error("the backup was not recorded as manually triggered")
	}

	// The artifact is really there, and is really the bytes the plugin wrote.
	reader, info, err := h.store.Get(ctx, b.GetArtifact().GetKey())
	if err != nil {
		t.Fatalf("Get artifact: %v", err)
	}
	defer func() { _ = reader.Close() }()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(stored, h.plugin.payload) {
		t.Errorf("the stored artifact is %d bytes and does not match what the plugin wrote", len(stored))
	}
	if info.Size != b.GetSizeBytes() {
		t.Errorf("the store reports %d bytes, the row says %d", info.Size, b.GetSizeBytes())
	}

	// The manifest is the evidence verification will compare against, so it has to survive the
	// round trip through JSONB intact.
	_, manifest, err := h.svc.GetBackup(ctx, backupID)
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if manifest.GetTotalRecords() != 160 || len(manifest.GetEntries()) != 2 {
		t.Errorf("manifest = %d records over %d entries, want 160 over 2",
			manifest.GetTotalRecords(), len(manifest.GetEntries()))
	}

	var jobState string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id = $1`, jobID).Scan(&jobState); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobState != "succeeded" {
		t.Errorf("job state = %q, want succeeded", jobState)
	}

	// The grant handed to the plugin must be scoped to this backup and carry no storage credential.
	target := h.plugin.lastTarget()
	if target == nil {
		t.Fatal("the plugin received no artifact target")
	}
	if !strings.Contains(target.GetObject().GetKey(), backupID) {
		t.Errorf("object key %q is not scoped to backup %s", target.GetObject().GetKey(), backupID)
	}
	if len(target.GetPartUrls()) != uploadParts {
		t.Errorf("the plugin was given %d part grants, want %d", len(target.GetPartUrls()), uploadParts)
	}
}

// TestFailedBackupLeavesNoArtifact is the guarantee the whole upload design rests on: a plugin that
// fails mid-stream has already written parts, and none of them may become a visible object. A
// partial artifact reporting success is a backup believed good and proven bad only at restore time.
func TestFailedBackupLeavesNoArtifact(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	h.plugin.payload = []byte(strings.Repeat("partial-", 12<<20/8))
	h.plugin.failAfterUpload = &fwv1.PluginError{
		Code:    fwv1.ErrorCode_ERROR_CODE_TOOL_FAILED,
		Message: "pg_dump exited with status 1: could not read block 42",
	}

	backupID, jobID, err := h.svc.RunBackup(ctx, RunBackupInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}

	b := h.waitForState(t, backupID)
	if b.GetState() != fwv1.BackupState_BACKUP_STATE_FAILED {
		t.Fatalf("state = %v, want FAILED", b.GetState())
	}
	if !strings.Contains(b.GetErrorMessage(), "could not read block 42") {
		t.Errorf("error_message = %q; the plugin's diagnosis should survive", b.GetErrorMessage())
	}

	key := artifactKeyFor(metadb.DefaultTenantID, h.instance, backupID)
	if _, err := h.store.Stat(ctx, key); !errors.Is(err, objstore.ErrNotFound) {
		t.Errorf("an artifact exists at %s for a failed backup (stat error = %v)", key, err)
	}

	var jobState string
	if err := h.pool.QueryRow(ctx, `SELECT state FROM jobs WHERE id = $1`, jobID).Scan(&jobState); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobState != "failed" {
		t.Errorf("job state = %q, want failed", jobState)
	}
}

// TestStreamWithoutATerminalMessageIsAFailure pins the one case core cannot interpret. A plugin that
// ends its stream without saying what happened may have finished or may have died half way, and the
// only safe reading is failure.
func TestStreamWithoutATerminalMessageIsAFailure(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	h.plugin.payload = []byte("some bytes")
	h.plugin.endWithoutTerminal = true

	backupID, _, err := h.svc.RunBackup(ctx, RunBackupInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}

	b := h.waitForState(t, backupID)
	if b.GetState() != fwv1.BackupState_BACKUP_STATE_FAILED {
		t.Fatalf("state = %v, want FAILED", b.GetState())
	}
	if !strings.Contains(b.GetErrorMessage(), "without reporting an outcome") {
		t.Errorf("error_message = %q", b.GetErrorMessage())
	}
}

// TestSecondBackupOfOneInstanceIsRefused checks the database-level backstop. Two simultaneous dumps
// of one production server is an operational incident, so it is prevented by a unique index rather
// than by careful code.
func TestSecondBackupOfOneInstanceIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	h.plugin.payload = []byte("slow artifact")
	h.plugin.block = make(chan struct{})

	first, _, err := h.svc.RunBackup(ctx, RunBackupInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("first RunBackup: %v", err)
	}

	if _, _, err := h.svc.RunBackup(ctx, RunBackupInput{InstanceID: h.instance}); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second RunBackup error = %v, want ErrAlreadyRunning", err)
	}

	close(h.plugin.block)
	if b := h.waitForState(t, first); b.GetState() != fwv1.BackupState_BACKUP_STATE_SUCCEEDED {
		t.Errorf("first backup ended %v: %s", b.GetState(), b.GetErrorMessage())
	}
}

// TestGetBackupRejectsAMalformedIdentifier keeps a typo in a URL from reaching a query.
func TestGetBackupRejectsAMalformedIdentifier(t *testing.T) {
	h := newHarness(t)
	if _, _, err := h.svc.GetBackup(testTenantCtx(), "not-a-uuid"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("GetBackup error = %v, want ErrInvalidArgument", err)
	}
	if _, _, err := h.svc.GetBackup(testTenantCtx(), "3f2504e0-4f89-11d3-9a0c-0305e82c3301"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetBackup error = %v, want ErrNotFound", err)
	}
}

// -----------------------------------------------------------------------------------------------
// Stubs
// -----------------------------------------------------------------------------------------------

// stubResolver answers with one instance and credentials that never leave this file.
type stubResolver struct{ instanceID string }

func (r *stubResolver) ResolveConnection(_ context.Context, instanceID string) (*inventory.Connection, error) {
	if instanceID != r.instanceID {
		return nil, fmt.Errorf("%w: instance %s", inventory.ErrNotFound, instanceID)
	}
	return &inventory.Connection{
		InstanceID: r.instanceID,
		EngineType: engine,
		Ref:        &fwv1.ConnectionRef{InstanceId: r.instanceID, TenantId: metadb.DefaultTenantID},
		Credentials: &fwv1.Credentials{
			Host: "db.example.internal", Port: 5432, Username: "fleetward", Database: "app",
		},
	}, nil
}

// stubRouter serves one engine type with one client.
type stubRouter struct {
	client fwv1.EnginePluginClient
	// history is what this engine declares about backups it did not take. Nil is the honest default
	// and the state every plugin starts in: it can see none.
	history *fwv1.BackupHistoryCapabilities
}

func (r *stubRouter) Client(engineType string) (fwv1.EnginePluginClient, *fwv1.Capabilities, error) {
	if engineType != engine {
		return nil, nil, fmt.Errorf("no plugin available for engine type %q", engineType)
	}
	return r.client, &fwv1.Capabilities{
		BackupHistory:        r.history,
		EngineType:           engine,
		PluginVersion:        "0.1.0",
		SupportsOnlineBackup: true,
		BackupMethods: []*fwv1.BackupMethod{{
			Id: "dump", Kind: fwv1.BackupKind_BACKUP_KIND_LOGICAL, IsDefault: true,
		}},
		SupportsSandboxRestore: true,
		SandboxTemplate: &fwv1.SandboxTemplate{
			ImageRepository: "testengine",
			DefaultTag:      "1",
			TagTemplate:     "{{ .Major }}",
			ContainerPort:   5432,
		},
		SupportedVerificationChecks: []fwv1.VerificationCheck{
			fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY,
			fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
		},
	}, nil
}

// stubEngine writes a canned payload through the grants core supplied, exactly as a real plugin
// does, so the upload path under test is the real one.
type stubEngine struct {
	fwv1.EnginePluginClient

	payload  []byte
	manifest *fwv1.SourceManifest
	// failAfterUpload makes the plugin report a terminal failure after writing parts, which is the
	// case that proves core discards them.
	failAfterUpload *fwv1.PluginError
	// endWithoutTerminal ends the stream saying nothing, which core must treat as a failure.
	endWithoutTerminal bool
	// block, when non-nil, holds the run open until it is closed.
	block chan struct{}

	// history is what this engine claims to see of backups Fleetward did not take, exercised by
	// observe_integration_test.go.
	history []*fwv1.ObservedBackup
	// externalID is what the engine calls the backup this plugin takes, reported back so that the
	// observation poll can converge on it rather than recording it twice.
	externalID string

	// The verification half, exercised by verify_integration_test.go.
	restoreError *fwv1.PluginError
	verifyResult *fwv1.VerifyRestoreResult
	verifyErr    error

	mu      sync.Mutex
	target  *fwv1.ArtifactTarget
	restore *fwv1.RestoreRequest
	verify  *fwv1.VerifyRestoreRequest
}

func (s *stubEngine) lastTarget() *fwv1.ArtifactTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.target
}

func (s *stubEngine) Backup(ctx context.Context, in *fwv1.BackupRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fwv1.BackupProgress], error) {
	s.mu.Lock()
	s.target = in.GetTarget()
	s.mu.Unlock()

	if s.block != nil {
		<-s.block
	}

	messages := []*fwv1.BackupProgress{{
		BackupId: in.GetBackupId(),
		Phase:    fwv1.JobPhase_JOB_PHASE_RUNNING,
		Message:  "working",
	}}

	parts, total, err := uploadPayload(ctx, in.GetTarget(), s.payload)
	if err != nil {
		return nil, err
	}

	switch {
	case s.endWithoutTerminal:
	case s.failAfterUpload != nil:
		messages = append(messages, &fwv1.BackupProgress{
			BackupId: in.GetBackupId(),
			Phase:    fwv1.JobPhase_JOB_PHASE_FAILED,
			Error:    s.failAfterUpload,
		})
	default:
		manifest := s.manifest
		if manifest == nil {
			manifest = &fwv1.SourceManifest{}
		}
		messages = append(messages, &fwv1.BackupProgress{
			BackupId: in.GetBackupId(),
			Phase:    fwv1.JobPhase_JOB_PHASE_COMPLETED,
			Result: &fwv1.BackupResult{
				Artifact:  in.GetTarget().GetObject(),
				SizeBytes: total,
				Checksum: &fwv1.Checksum{
					Algorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
					Value:     "0000000000000000000000000000000000000000000000000000000000000000",
				},
				MethodId: "dump",
				Manifest: manifest,
				Parts:    parts,
				// What the engine called this backup, for engines that keep their own record of
				// one. Empty for engines that do not, which is the default here.
				ExternalId: s.externalID,
			},
		})
	}

	return &stubStream{ctx: ctx, messages: messages}, nil
}

// uploadPayload writes the payload through the presigned part grants, the way a plugin does.
func uploadPayload(ctx context.Context, target *fwv1.ArtifactTarget, payload []byte) ([]*fwv1.UploadedPart, int64, error) {
	partSize := int(target.GetPartSizeBytes())
	var parts []*fwv1.UploadedPart

	for offset, number := 0, 0; offset < len(payload); number++ {
		end := min(offset+partSize, len(payload))
		chunk := payload[offset:end]

		grant := target.GetPartUrls()[number]
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, grant.GetUrl(), bytes.NewReader(chunk))
		if err != nil {
			return nil, 0, err
		}
		req.ContentLength = int64(len(chunk))

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, 0, fmt.Errorf("part %d rejected with %s: %s", number+1, resp.Status, body)
		}

		parts = append(parts, &fwv1.UploadedPart{
			PartNumber: int32(number + 1),
			Etag:       resp.Header.Get("ETag"),
			SizeBytes:  int64(len(chunk)),
		})
		offset = end
	}
	return parts, int64(len(payload)), nil
}

// stubStream replays a fixed sequence of progress messages.
type stubStream struct {
	ctx      context.Context
	messages []*fwv1.BackupProgress
	next     int
}

func (s *stubStream) Recv() (*fwv1.BackupProgress, error) {
	if s.next >= len(s.messages) {
		return nil, io.EOF
	}
	msg := s.messages[s.next]
	s.next++
	return msg, nil
}

func (s *stubStream) Header() (metadata.MD, error) { return nil, nil }
func (s *stubStream) Trailer() metadata.MD         { return nil }
func (s *stubStream) CloseSend() error             { return nil }
func (s *stubStream) Context() context.Context     { return s.ctx }
func (s *stubStream) SendMsg(any) error            { return nil }
func (s *stubStream) RecvMsg(any) error            { return io.EOF }
