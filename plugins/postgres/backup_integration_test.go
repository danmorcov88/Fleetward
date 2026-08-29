//go:build integration

// Integration tests for the pg_dump backup method, against a real PostgreSQL and a real
// S3-compatible store.
//
// The test plays core's part of the protocol: it begins a multipart upload, hands the plugin the
// presigned part grants, and completes the upload from the receipts the plugin returns. That is
// exactly what internal/controlplane/backup does, so what is proven here is the contract between
// the two halves rather than a mock of it.
//
// Run with: go test -tags=integration ./plugins/postgres/...
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
)

const (
	minioImage  = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	minioUser   = "fleetward"
	minioSecret = "fleetward-integration"
	testBucket  = "fleetward-backups"
	// testPartSize is the smallest S3 permits for a non-final part, which keeps the presigned grants
	// cheap. A seeded test database dumps to well under one part; multi-part assembly against a real
	// store is covered by the control plane's own integration tests.
	testPartSize = objstore.MinPartSizeBytes
)

// requirePgDump asserts a usable client is on PATH.
//
// This is the one test in the suite that needs a host tool rather than only Docker: the plugin
// orchestrates pg_dump by design, so a test that mocked it away would prove nothing about the
// thing under test. CI installs the client explicitly; locally the message says what to install.
func requirePgDump(t *testing.T, serverMajor int) {
	t.Helper()

	path, err := exec.LookPath(dumpTool)
	if err != nil {
		t.Skipf("pg_dump is not on PATH; install postgresql-client-%d to run this test", serverMajor)
	}

	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatalf("run pg_dump --version: %v", err)
	}
	match := regexp.MustCompile(`(\d+)\.`).FindStringSubmatch(string(out))
	if match == nil {
		t.Fatalf("could not read a major version from %q", out)
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse pg_dump version from %q: %v", out, err)
	}
	// pg_dump refuses to dump a server newer than itself, and reporting that as a product failure
	// would send someone hunting a bug in the plugin.
	if major < serverMajor {
		t.Skipf("pg_dump is version %d but the test server is %d; install postgresql-client-%d",
			major, serverMajor, serverMajor)
	}
}

// startMinIO brings up an S3-compatible store and returns a client for it with the bucket created.
func startMinIO(t *testing.T) objstore.ObjectStore {
	t.Helper()

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
		AccessKey:     minioUser,
		SecretKey:     minioSecret,
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

// collectBackup drives the plugin's Backup RPC and returns the terminal message.
func collectBackup(ctx context.Context, req *fwv1.BackupRequest) (*fwv1.BackupProgress, []*fwv1.BackupProgress, error) {
	var (
		all      []*fwv1.BackupProgress
		terminal *fwv1.BackupProgress
	)
	err := New().Backup(ctx, req, func(p *fwv1.BackupProgress) error {
		all = append(all, p)
		switch p.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED, fwv1.JobPhase_JOB_PHASE_FAILED:
			terminal = p
		}
		return nil
	})
	return terminal, all, err
}

// TestBackupAgainstRealPostgres is the slice's headline test: a real dump of a real database lands
// in a real object store, and the manifest describes what was actually in it.
func TestBackupAgainstRealPostgres(t *testing.T) {
	requirePgDump(t, 16)

	creds := startPostgres(t)
	seed(t, creds)
	store := startMinIO(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const backupID = "0d8b0b3e-6d4a-4f1f-9c1e-2f5a1d4b8c90"
	key := objstore.ArtifactKey("tenant", "instance", backupID, "artifact")

	upload, err := store.CreateMultipartUpload(ctx, key, 64, time.Hour)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	terminal, progress, err := collectBackup(ctx, &fwv1.BackupRequest{
		Credentials: creds,
		BackupId:    backupID,
		Target:      artifactTarget(key, upload),
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if terminal == nil {
		t.Fatal("the stream ended without a terminal message")
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("phase = %s: %s", terminal.GetPhase(), terminal.GetError().GetMessage())
	}

	// The plugin must report progress, not only an outcome: without it a long backup is
	// indistinguishable from a hung one.
	if len(progress) < 2 {
		t.Errorf("the plugin emitted %d messages; progress should precede the result", len(progress))
	}

	result := terminal.GetResult()
	if result.GetSizeBytes() == 0 {
		t.Fatal("the backup reported a zero-byte artifact")
	}
	if len(result.GetParts()) == 0 {
		t.Fatal("no part receipts were reported; core could not complete the upload")
	}
	if result.GetMethodId() != MethodPgDump {
		t.Errorf("method_id = %q, want %q", result.GetMethodId(), MethodPgDump)
	}
	if result.GetEngineVersion() == "" {
		t.Error("the result records no engine version; a restore could not be matched to an image")
	}
	if result.GetConsistencyPoint() == nil {
		t.Error("the result records no consistency point")
	}
	if result.GetMetadata()["format"] != formatCustom {
		t.Errorf("metadata[format] = %q, want %q", result.GetMetadata()["format"], formatCustom)
	}

	// Core's half of the protocol: assemble the parts the plugin wrote.
	parts := make([]objstore.CompletedPart, 0, len(result.GetParts()))
	for _, p := range result.GetParts() {
		parts = append(parts, objstore.CompletedPart{PartNumber: int(p.GetPartNumber()), ETag: p.GetEtag()})
	}
	info, err := store.CompleteMultipartUpload(ctx, key, upload.UploadID, parts)
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}
	if info.Size != result.GetSizeBytes() {
		t.Errorf("stored artifact is %d bytes, the plugin reported %d", info.Size, result.GetSizeBytes())
	}

	// The checksum has to describe the bytes that actually landed, because it is the only thing
	// standing between a corrupted artifact and a restore that silently loads it.
	reader, _, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get artifact: %v", err)
	}
	defer func() { _ = reader.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != result.GetChecksum().GetValue() {
		t.Errorf("artifact checksum = %s, the plugin reported %s", got, result.GetChecksum().GetValue())
	}
	if result.GetChecksum().GetAlgorithm() != fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256 {
		t.Errorf("checksum algorithm = %v, want SHA256", result.GetChecksum().GetAlgorithm())
	}

	// The manifest is the reason verification can mean anything. seed() creates two tables with
	// known contents, so exact counts are checkable rather than merely plausible.
	manifest := result.GetManifest()
	if manifest.GetIsSampled() {
		t.Error("counts are exact, so is_sampled must be false")
	}
	counts := map[string]int64{}
	for _, entry := range manifest.GetEntries() {
		if entry.GetDatabase() != testDB {
			t.Errorf("entry %q records database %q, want %q", entry.GetObjectName(), entry.GetDatabase(), testDB)
		}
		counts[entry.GetObjectName()] = entry.GetRecordCount()
	}
	for name, want := range map[string]int64{"public.customers": 40, "public.orders": 120} {
		if got, ok := counts[name]; !ok {
			t.Errorf("the manifest has no entry for %s", name)
		} else if got != want {
			t.Errorf("%s recorded %d rows, want %d", name, got, want)
		}
	}
	if manifest.GetTotalObjects() != int64(len(manifest.GetEntries())) {
		t.Errorf("total_objects = %d but there are %d entries",
			manifest.GetTotalObjects(), len(manifest.GetEntries()))
	}
	var sum int64
	for _, entry := range manifest.GetEntries() {
		sum += entry.GetRecordCount()
	}
	if manifest.GetTotalRecords() != sum {
		t.Errorf("total_records = %d but the entries sum to %d", manifest.GetTotalRecords(), sum)
	}
}

// TestManifestCountsComeFromTheExportedSnapshot is the reason the snapshot exists.
//
// Rows are committed by a second connection at the one moment that matters: after the backup has
// exported its snapshot and before it counts. Repeatable read means those rows must be invisible to
// the count, and pg_dump is handed that same snapshot — so the manifest and the artifact describe
// one consistent point. Without this, a manifest counted against a live database would disagree
// with its own artifact, and slice A5 would report a false verification failure on a good backup.
//
// The write is driven from the plugin's own progress stream rather than from a timer, so the
// ordering is guaranteed rather than likely.
func TestManifestCountsComeFromTheExportedSnapshot(t *testing.T) {
	requirePgDump(t, 16)

	creds := startPostgres(t)
	seed(t, creds)
	store := startMinIO(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const backupID = "5c1a1f8e-1b2c-4d3e-8f90-a1b2c3d4e5f6"
	key := objstore.ArtifactKey("tenant", "instance", backupID, "artifact")
	upload, err := store.CreateMultipartUpload(ctx, key, 64, time.Hour)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	writer, err := connect(ctx, creds)
	if err != nil {
		t.Fatalf("connect the concurrent writer: %v", err)
	}
	defer func() { _ = writer.Close(context.Background()) }()

	var (
		terminal *fwv1.BackupProgress
		inserted bool
	)
	err = New().Backup(ctx, &fwv1.BackupRequest{
		Credentials: creds,
		BackupId:    backupID,
		Target:      artifactTarget(key, upload),
	}, func(p *fwv1.BackupProgress) error {
		// This message is emitted after pg_export_snapshot() and before the counting queries run.
		// Committing here — synchronously, before the callback returns — puts the writes strictly
		// inside the window the snapshot has to hide.
		if !inserted && strings.HasPrefix(p.GetMessage(), "counting rows in") {
			inserted = true
			if _, err := writer.Exec(ctx,
				`INSERT INTO customers (name) SELECT 'late-' || g FROM generate_series(1, 25) g`); err != nil {
				return fmt.Errorf("concurrent insert: %w", err)
			}
		}
		switch p.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED, fwv1.JobPhase_JOB_PHASE_FAILED:
			terminal = p
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !inserted {
		t.Fatal("the plugin never announced that it was counting; the test proved nothing")
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("phase = %s: %s", terminal.GetPhase(), terminal.GetError().GetMessage())
	}

	// The 25 rows committed after the snapshot must not appear.
	var live int64
	if err := writer.QueryRow(ctx, `SELECT count(*) FROM customers`).Scan(&live); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if live != 65 {
		t.Fatalf("the concurrent writer committed %d rows in total, want 65", live)
	}

	for _, entry := range terminal.GetResult().GetManifest().GetEntries() {
		if entry.GetObjectName() != "public.customers" {
			continue
		}
		if entry.GetRecordCount() != 40 {
			t.Errorf("public.customers recorded %d rows, want the 40 visible in the exported snapshot",
				entry.GetRecordCount())
		}
		return
	}
	t.Error("the manifest has no entry for public.customers")
}

// TestBackupFailsCleanlyOnAMissingDatabase asserts the terminal-failure contract: a run that cannot
// succeed reports a typed error on the stream and completes no upload, so no artifact can ever be
// mistaken for a good one.
func TestBackupFailsCleanlyOnAMissingDatabase(t *testing.T) {
	requirePgDump(t, 16)

	creds := startPostgres(t)
	store := startMinIO(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const backupID = "9a7b6c5d-4e3f-4a2b-9c8d-7e6f5a4b3c2d"
	key := objstore.ArtifactKey("tenant", "instance", backupID, "artifact")
	upload, err := store.CreateMultipartUpload(ctx, key, 8, time.Hour)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	terminal, _, err := collectBackup(ctx, &fwv1.BackupRequest{
		Credentials: creds,
		BackupId:    backupID,
		Databases:   []string{"a_database_that_does_not_exist"},
		Target:      artifactTarget(key, upload),
	})
	if err != nil {
		t.Fatalf("Backup returned an RPC error instead of a terminal failure: %v", err)
	}
	if terminal == nil || terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", terminal.GetPhase())
	}
	if terminal.GetError().GetCode() == fwv1.ErrorCode_ERROR_CODE_UNSPECIFIED {
		t.Error("the failure carries no structured code; core could not classify it")
	}
	if terminal.GetResult() != nil {
		t.Error("a failed backup must not carry a result")
	}

	// Nothing was completed, so nothing is visible in the bucket.
	if _, err := store.Stat(ctx, key); err == nil {
		t.Error("an artifact exists for a backup that failed")
	}
	if err := store.AbortMultipartUpload(ctx, key, upload.UploadID); err != nil {
		t.Errorf("AbortMultipartUpload: %v", err)
	}
}

// TestBackupRejectsAnUnknownMethod checks that a method the plugin does not implement is refused
// before anything touches the database.
func TestBackupRejectsAnUnknownMethod(t *testing.T) {
	ctx := context.Background()
	terminal, _, err := collectBackup(ctx, &fwv1.BackupRequest{
		Credentials: &fwv1.Credentials{Host: "127.0.0.1", Port: 1, Username: "u", Database: "app"},
		BackupId:    "1111aaaa-2222-bbbb-3333-cccc4444dddd",
		MethodId:    "pgbackrest",
		Target: &fwv1.ArtifactTarget{
			Object:        &fwv1.ObjectRef{Bucket: testBucket, Key: "unused"},
			PartUrls:      []*fwv1.PresignedURL{{Url: "https://example.invalid", Method: "PUT"}},
			PartSizeBytes: testPartSize,
		},
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", terminal.GetPhase())
	}
	if code := terminal.GetError().GetCode(); code != fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Errorf("code = %v, want INVALID_ARGUMENT", code)
	}
}

// artifactTarget renders an upload as the grant a plugin receives.
func artifactTarget(key string, upload objstore.MultipartUpload) *fwv1.ArtifactTarget {
	parts := make([]*fwv1.PresignedURL, 0, len(upload.Parts))
	for _, grant := range upload.Parts {
		parts = append(parts, &fwv1.PresignedURL{Url: grant.URL, Method: grant.Method})
	}
	return &fwv1.ArtifactTarget{
		Object:            &fwv1.ObjectRef{Bucket: testBucket, Key: key},
		PartUrls:          parts,
		PartSizeBytes:     upload.PartSize,
		ChecksumAlgorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
	}
}
