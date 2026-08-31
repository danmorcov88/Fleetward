//go:build integration

// Integration tests for the restore-and-verify half of the loop, end to end against real servers.
//
// This is the test the slice exists for: a real database is dumped, the artifact lands in a real
// object store, it is downloaded and restored into a second real PostgreSQL, and the restored copy
// is compared row for row against the manifest captured when the dump was taken. Nothing here is
// mocked, because a verification that has only been proven against a mock has proven nothing.
//
// Run with: go test -tags=integration ./plugins/postgres/...
package postgres

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
	"time"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/plugin/sdk"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
)

// requirePgRestore asserts a usable client is on PATH, the same way requirePgDump does.
//
// pg_restore refuses nothing on version grounds the way pg_dump does, but an old client cannot read
// an archive written by a newer pg_dump, so the same floor applies.
func requirePgRestore(t *testing.T, serverMajor int) {
	t.Helper()

	path, err := exec.LookPath(restoreTool)
	if err != nil {
		t.Skipf("pg_restore is not on PATH; install postgresql-client-%d to run this test", serverMajor)
	}

	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		t.Fatalf("run pg_restore --version: %v", err)
	}
	match := regexp.MustCompile(`(\d+)\.`).FindStringSubmatch(string(out))
	if match == nil {
		t.Fatalf("could not read a major version from %q", out)
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatalf("parse pg_restore version from %q: %v", out, err)
	}
	if major < serverMajor {
		t.Skipf("pg_restore is version %d but the artifact is from %d; install postgresql-client-%d",
			major, serverMajor, serverMajor)
	}
}

// backupToStore runs a real backup and returns the artifact's coordinates and its manifest, which is
// exactly the state slice A4 leaves behind in the backups table.
func backupToStore(t *testing.T, ctx context.Context, creds *fwv1.Credentials, store objstore.ObjectStore) (key string, result *fwv1.BackupResult) {
	t.Helper()

	const backupID = "5c1b6f0e-2d3a-4b8c-9f10-6a7b8c9d0e1f"
	key = objstore.ArtifactKey("tenant", "instance", backupID, "artifact")

	upload, err := store.CreateMultipartUpload(ctx, key, 64, time.Hour)
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	terminal, _, err := collectBackup(ctx, &fwv1.BackupRequest{
		Credentials: creds,
		BackupId:    backupID,
		Target:      artifactTarget(key, upload),
	})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("backup phase = %s: %s", terminal.GetPhase(), terminal.GetError().GetMessage())
	}
	result = terminal.GetResult()

	parts := make([]objstore.CompletedPart, 0, len(result.GetParts()))
	for _, p := range result.GetParts() {
		parts = append(parts, objstore.CompletedPart{PartNumber: int(p.GetPartNumber()), ETag: p.GetEtag()})
	}
	if _, err := store.CompleteMultipartUpload(ctx, key, upload.UploadID, parts); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	return key, result
}

// artifactSource builds what core hands the plugin: a presigned download grant and the checksum the
// backup recorded. A GET has none of the Content-Length trouble ADR-0021 describes, so one URL is
// enough.
func artifactSource(t *testing.T, ctx context.Context, store objstore.ObjectStore, key string, result *fwv1.BackupResult) *fwv1.ArtifactSource {
	t.Helper()

	grant, err := store.PresignGet(ctx, key, time.Hour)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	return &fwv1.ArtifactSource{
		Object:      &fwv1.ObjectRef{Bucket: testBucket, Key: key},
		DownloadUrl: &fwv1.PresignedURL{Url: grant.URL, Method: grant.Method},
		Checksum:    result.GetChecksum(),
		Role:        fwv1.ArtifactRole_ARTIFACT_ROLE_BASE,
		SizeBytes:   result.GetSizeBytes(),
	}
}

// collectRestore drives the plugin's Restore RPC and returns the terminal message.
func collectRestore(ctx context.Context, req *fwv1.RestoreRequest) (*fwv1.RestoreProgress, []*fwv1.RestoreProgress, error) {
	var (
		all      []*fwv1.RestoreProgress
		terminal *fwv1.RestoreProgress
	)
	err := New().Restore(ctx, req, func(p *fwv1.RestoreProgress) error {
		all = append(all, p)
		switch p.GetPhase() {
		case fwv1.JobPhase_JOB_PHASE_COMPLETED, fwv1.JobPhase_JOB_PHASE_FAILED:
			terminal = p
		}
		return nil
	})
	return terminal, all, err
}

// sandboxTarget describes a restore into a throwaway instance, which is what core builds from a
// provisioned sandbox's credentials.
func sandboxTarget(creds *fwv1.Credentials) *fwv1.RestoreTarget {
	return &fwv1.RestoreTarget{
		Kind:        fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX,
		Credentials: creds,
		SandboxId:   "integration-sandbox",
	}
}

// TestRestoreAndVerifyAgainstRealPostgres is the whole loop: dump, upload, download, restore, count.
func TestRestoreAndVerifyAgainstRealPostgres(t *testing.T) {
	requirePgDump(t, 16)
	requirePgRestore(t, 16)

	source := startPostgres(t)
	seed(t, source)
	store := startMinIO(t)
	// A second real PostgreSQL, empty, standing in for the container core's SandboxProvider would
	// have provisioned. What matters to this test is that it is a genuine server of the matching
	// version, which is what makes the restore prove anything.
	sandbox := startPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	key, result := backupToStore(t, ctx, source, store)

	terminal, progress, err := collectRestore(ctx, &fwv1.RestoreRequest{
		RestoreId: "b8f0d1c2-3e4a-5b6c-7d8e-9f0a1b2c3d4e",
		Artifacts: []*fwv1.ArtifactSource{artifactSource(t, ctx, store, key, result)},
		Target:    sandboxTarget(sandbox),
		MethodId:  MethodPgDump,
		// The backup's own metadata, handed straight back by core. It is how the plugin knows the
		// artifact is a pg_restore archive rather than a psql script.
		Options: result.GetMetadata(),
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if terminal == nil {
		t.Fatal("the restore stream ended without a terminal message")
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("restore phase = %s: %s", terminal.GetPhase(), terminal.GetError().GetMessage())
	}
	if len(progress) < 2 {
		t.Errorf("the plugin emitted %d messages; progress should precede the result", len(progress))
	}
	if got := terminal.GetResult().GetRestoredDatabases(); len(got) != 1 || got[0] != sandbox.GetDatabase() {
		t.Errorf("restored_databases = %v, want [%s]", got, sandbox.GetDatabase())
	}
	if terminal.GetResult().GetDuration().AsDuration() <= 0 {
		t.Error("the restore reports no duration")
	}

	verified, err := New().VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
		VerificationId: "c9a1e2f3-4b5c-6d7e-8f90-a1b2c3d4e5f6",
		Target:         sandboxTarget(sandbox),
		Expected:       result.GetManifest(),
	})
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}

	if verified.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
		t.Fatalf("status = %v, want VERIFIED\n%s", verified.GetStatus(), verified.GetReport())
	}
	if len(verified.GetChecks()) != len(supportedChecks()) {
		t.Errorf("ran %d checks, want the %d this plugin declares",
			len(verified.GetChecks()), len(supportedChecks()))
	}
	for _, check := range verified.GetChecks() {
		if !check.GetPassed() {
			t.Errorf("%v failed on a healthy restore: %s", check.GetCheck(), check.GetMessage())
		}
	}
	if verified.GetReport() == "" {
		t.Error("no report was produced; the UI has nothing to show")
	}
	if verified.GetDuration().AsDuration() <= 0 {
		t.Error("the verification reports no duration")
	}
}

// TestVerifyReportsTheRowsThatAreMissing proves the failure answer is specific rather than a
// boolean. The restored copy here is genuinely correct; the manifest is not — which is the same
// comparison a truncated artifact would fail, without needing to corrupt one (that is slice A6).
func TestVerifyReportsTheRowsThatAreMissing(t *testing.T) {
	requirePgDump(t, 16)
	requirePgRestore(t, 16)

	source := startPostgres(t)
	seed(t, source)
	store := startMinIO(t)
	sandbox := startPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	key, result := backupToStore(t, ctx, source, store)

	terminal, _, err := collectRestore(ctx, &fwv1.RestoreRequest{
		RestoreId: "d0b2f3a4-5c6d-7e8f-9012-b3c4d5e6f708",
		Artifacts: []*fwv1.ArtifactSource{artifactSource(t, ctx, store, key, result)},
		Target:    sandboxTarget(sandbox),
		Options:   result.GetMetadata(),
	})
	if err != nil || terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_COMPLETED {
		t.Fatalf("Restore: %v / %s", err, terminal.GetError().GetMessage())
	}

	// Claim one more row than the source ever held, plus a table that never existed.
	expected := result.GetManifest()
	for _, entry := range expected.GetEntries() {
		if entry.GetObjectName() == "public.orders" {
			entry.RecordCount++
		}
	}
	expected.Entries = append(expected.Entries, &fwv1.ManifestEntry{
		Database:    expected.GetEntries()[0].GetDatabase(),
		ObjectName:  "public.ghost",
		RecordCount: 9,
	})

	verified, err := New().VerifyRestore(ctx, &fwv1.VerifyRestoreRequest{
		VerificationId: "e1c3a4b5-6d7e-8f90-1234-c5d6e7f80912",
		Target:         sandboxTarget(sandbox),
		Expected:       expected,
	})
	if err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}

	if verified.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED\n%s", verified.GetStatus(), verified.GetReport())
	}

	var sawShortTable, sawMissingTable bool
	for _, check := range verified.GetChecks() {
		for _, d := range check.GetDiscrepancies() {
			switch d.GetObjectName() {
			case "public.orders":
				sawShortTable = true
				if d.GetActual() != d.GetExpected()-1 {
					t.Errorf("orders discrepancy = %d → %d, want a difference of exactly one row",
						d.GetExpected(), d.GetActual())
				}
			case "public.ghost":
				sawMissingTable = true
			}
		}
	}
	if !sawShortTable {
		t.Error("the row-count difference was not reported per object")
	}
	if !sawMissingTable {
		t.Error("the missing table was not reported")
	}
}

// TestRestoreRefusesAnArtifactThatFailsItsChecksum is the check that has to happen before a single
// statement is applied: restoring a bad artifact and only then noticing the counts are wrong wastes
// minutes and reports the wrong cause.
func TestRestoreRefusesAnArtifactThatFailsItsChecksum(t *testing.T) {
	requirePgDump(t, 16)
	requirePgRestore(t, 16)

	source := startPostgres(t)
	seed(t, source)
	store := startMinIO(t)
	sandbox := startPostgres(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	key, result := backupToStore(t, ctx, source, store)

	artifact := artifactSource(t, ctx, store, key, result)
	artifact.Checksum = &fwv1.Checksum{
		Algorithm: fwv1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_SHA256,
		Value:     "1111111111111111111111111111111111111111111111111111111111111111",
	}

	terminal, _, err := collectRestore(ctx, &fwv1.RestoreRequest{
		RestoreId: "f2d4b5c6-7e8f-9012-3456-d7e8f9081a2b",
		Artifacts: []*fwv1.ArtifactSource{artifact},
		Target:    sandboxTarget(sandbox),
		Options:   result.GetMetadata(),
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if terminal.GetPhase() != fwv1.JobPhase_JOB_PHASE_FAILED {
		t.Fatalf("phase = %s, want FAILED for a checksum mismatch", terminal.GetPhase())
	}
	if !sdk.IsArtifactCorrupt(terminal.GetError()) {
		t.Errorf("the failure does not blame the artifact, so core would report it as inconclusive: %v",
			terminal.GetError())
	}

	// Nothing may have been applied: the sandbox is still empty.
	conn, err := connect(ctx, sandbox)
	if err != nil {
		t.Fatalf("connect to the sandbox: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	tables, err := listTables(ctx, conn)
	if err != nil {
		t.Fatalf("listTables: %v", err)
	}
	if len(tables) != 0 {
		t.Errorf("the sandbox holds %d tables; a rejected artifact was partly applied", len(tables))
	}
}
