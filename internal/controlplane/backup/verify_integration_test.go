//go:build integration

// Integration tests for the verification half of the loop, against a real metadata store and a real
// object store.
//
// The plugin and the container runtime are stubs, deliberately. What core owns here is the
// orchestration and the bookkeeping: pick the sandbox image from the version that produced the
// artifact, mint a download grant, drive two RPCs, destroy the sandbox on every path, and write down
// a conclusion that distinguishes "this backup is bad" from "we could not tell". The PostgreSQL
// plugin proves the other half against a real server in plugins/postgres, and internal/controlplane/
// sandbox proves that containers really start and are really destroyed.
//
// Run with: go test -tags=integration ./internal/controlplane/backup/...
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/sandbox"
)

// seedBackup inserts a succeeded backup with everything a verification needs, so the verification
// tests do not have to run a backup first.
func seedBackup(t *testing.T, ctx context.Context, h *harness, mutate func(*seededBackup)) string {
	t.Helper()

	b := seededBackup{
		state:         "succeeded",
		objectKey:     "artifacts/seeded/artifact",
		checksum:      strings.Repeat("a", 64),
		engineVersion: "16.2",
		manifest: `{"entries":[{"database":"app","objectName":"public.orders","recordCount":"120"}],` +
			`"totalObjects":"1","totalRecords":"120"}`,
		metadata: `{"format":"custom","database":"app"}`,
	}
	if mutate != nil {
		mutate(&b)
	}

	var backupID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, method_id, state, bucket, object_key, size_bytes,
		                     checksum_algorithm, checksum_value, engine_version, manifest, metadata,
		                     started_at, completed_at)
		VALUES ($1, $2, 'dump', $3, $4, $5, 4096, 'CHECKSUM_ALGORITHM_SHA256', $6, $7, $8, $9, now(), now())
		RETURNING id`,
		"00000000-0000-0000-0000-000000000001", h.instance, b.state, testBucket, b.objectKey,
		b.checksum, b.engineVersion, b.manifest, b.metadata).Scan(&backupID); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	return backupID
}

type seededBackup struct {
	state         string
	objectKey     string
	checksum      string
	engineVersion string
	manifest      string
	metadata      string
}

// waitForVerification polls until the verification reaches a conclusion, which is how a real caller
// observes an asynchronous run.
func (h *harness) waitForVerification(t *testing.T, verificationID string) *fwv1.Verification {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		v, err := h.svc.GetVerification(testTenantCtx(), verificationID)
		if err != nil {
			t.Fatalf("GetVerification: %v", err)
		}
		if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_UNSPECIFIED {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("verification %s never reached a conclusion", verificationID)
	return nil
}

// TestRunVerificationRecordsAVerifiedBackup is the slice's headline: a backup restored into a
// sandbox, compared against its manifest, and written down as proven.
func TestRunVerificationRecordsAVerifiedBackup(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	backupID := seedBackup(t, ctx, h, nil)
	h.plugin.verifyResult = &fwv1.VerifyRestoreResult{
		Status: fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED,
		Checks: []*fwv1.CheckResult{
			{Check: fwv1.VerificationCheck_VERIFICATION_CHECK_CONNECTIVITY, Passed: true, Message: "up"},
			{Check: fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS, Passed: true, Message: "120 rows match"},
		},
		Report: "compared 1 object against the backup manifest",
	}

	verificationID, jobID, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
	if err != nil {
		t.Fatalf("RunVerification: %v", err)
	}

	v := h.waitForVerification(t, verificationID)
	if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
		t.Fatalf("status = %v: %s", v.GetStatus(), v.GetErrorMessage())
	}
	if len(v.GetChecks()) != 2 {
		t.Errorf("recorded %d checks, want 2", len(v.GetChecks()))
	}
	// Timestamps rather than the duration: with a stubbed plugin and a stubbed runtime the whole
	// verification finishes in microseconds, so duration_ms legitimately rounds to zero. A real one
	// spends a minute pulling an image.
	if v.GetStartedAt() == nil || v.GetCompletedAt() == nil {
		t.Error("the verification was not timestamped")
	}
	if v.GetReport() == "" {
		t.Error("no report was recorded; the UI has nothing to show")
	}

	// The plugin must have been handed the artifact, its checksum, and the manifest to compare
	// against — without the manifest, verification degrades to "did it start".
	restore := h.plugin.lastRestore()
	if restore == nil {
		t.Fatal("the plugin was never asked to restore")
	}
	if got := restore.GetArtifacts()[0].GetChecksum().GetValue(); got == "" {
		t.Error("the artifact was handed over without its checksum, so corruption could not be detected")
	}
	if restore.GetArtifacts()[0].GetDownloadUrl().GetUrl() == "" {
		t.Error("the artifact carries no download grant")
	}
	if restore.GetTarget().GetKind() != fwv1.RestoreTargetKind_RESTORE_TARGET_KIND_SANDBOX {
		t.Errorf("target kind = %v, want SANDBOX", restore.GetTarget().GetKind())
	}
	// The backup's own metadata is handed straight back: which tool loads an artifact is engine
	// knowledge core must not acquire.
	if restore.GetOptions()["format"] != "custom" {
		t.Errorf("restore options = %v, want the backup's recorded metadata", restore.GetOptions())
	}

	verify := h.plugin.lastVerify()
	if verify.GetExpected().GetTotalRecords() != 120 {
		t.Errorf("the plugin was given a manifest of %d records, want 120",
			verify.GetExpected().GetTotalRecords())
	}

	// The sandbox image is resolved from the version that produced the artifact, not from whatever
	// the instance runs today.
	if got := h.sandboxes.lastSpec().EngineVersion; got != "16.2" {
		t.Errorf("sandbox engine version = %q, want the backup's 16.2", got)
	}

	var sandboxImage string
	if err := h.pool.QueryRow(ctx, `SELECT sandbox_image FROM verifications WHERE id = $1`,
		verificationID).Scan(&sandboxImage); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	if sandboxImage != "testengine:16" {
		t.Errorf("sandbox_image = %q, want testengine:16", sandboxImage)
	}

	var jobState, jobKind string
	if err := h.pool.QueryRow(ctx, `SELECT state, kind FROM jobs WHERE id = $1`, jobID).
		Scan(&jobState, &jobKind); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobState != "succeeded" || jobKind != "verify" {
		t.Errorf("job = %s/%s, want succeeded/verify", jobKind, jobState)
	}

	// Cleanup is the guarantee this whole design rests on.
	h.sandboxes.assertNoneLeaked(t)
}

// TestAFailedVerificationIsRecordedWithItsDiscrepancies is the answer the product exists to
// produce. A backup that succeeded and cannot be restored to what it claimed is more dangerous than
// a backup that is missing, and the per-table numbers are the only actionable part.
func TestAFailedVerificationIsRecordedWithItsDiscrepancies(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	backupID := seedBackup(t, ctx, h, nil)
	h.plugin.verifyResult = &fwv1.VerifyRestoreResult{
		Status: fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED,
		Checks: []*fwv1.CheckResult{{
			Check:    fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS,
			Passed:   false,
			Severity: fwv1.Severity_SEVERITY_CRITICAL,
			Message:  "1 object holds the wrong number of rows",
			Discrepancies: []*fwv1.Discrepancy{{
				Database: "app", ObjectName: "public.orders", Expected: 120, Actual: 118,
			}},
		}},
		Report: "public.orders: expected 120, found 118",
	}

	verificationID, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
	if err != nil {
		t.Fatalf("RunVerification: %v", err)
	}

	v := h.waitForVerification(t, verificationID)
	if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED", v.GetStatus())
	}

	d := v.GetChecks()[0].GetDiscrepancies()
	if len(d) != 1 || d[0].GetExpected() != 120 || d[0].GetActual() != 118 {
		t.Fatalf("the discrepancy did not survive persistence: %v", d)
	}

	// The row has to be readable the way an operator will read it, which is with psql.
	var status string
	var checkCount int
	if err := h.pool.QueryRow(ctx, `
		SELECT status, jsonb_array_length(checks) FROM verifications WHERE id = $1`,
		verificationID).Scan(&status, &checkCount); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	if status != "failed" || checkCount != 1 {
		t.Errorf("row = %s with %d checks, want failed with 1", status, checkCount)
	}

	// A failed verification is still a job that did its work. Collapsing the two would make a bad
	// backup indistinguishable from a broken control plane.
	var jobState string
	if err := h.pool.QueryRow(ctx, `
		SELECT j.state FROM jobs j JOIN verifications v ON v.job_id = j.id WHERE v.id = $1`,
		verificationID).Scan(&jobState); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if jobState != "succeeded" {
		t.Errorf("job state = %q; the verification ran to a conclusion, so the job succeeded", jobState)
	}

	h.sandboxes.assertNoneLeaked(t)
}

// TestACorruptArtifactFailsRatherThanBeingInconclusive is the path slice A6 will drive end to end.
// The plugin blames the artifact itself, and core must read that as data loss rather than as
// machinery trouble.
func TestACorruptArtifactFailsRatherThanBeingInconclusive(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	backupID := seedBackup(t, ctx, h, nil)
	h.plugin.restoreError = sdkArtifactCorrupt("the artifact does not match its checksum")

	verificationID, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
	if err != nil {
		t.Fatalf("RunVerification: %v", err)
	}

	v := h.waitForVerification(t, verificationID)
	if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_FAILED {
		t.Fatalf("status = %v, want FAILED for a corrupted artifact", v.GetStatus())
	}
	if !strings.Contains(v.GetReport(), "checksum") {
		t.Errorf("the report does not say what went wrong: %q", v.GetReport())
	}
	h.sandboxes.assertNoneLeaked(t)
}

// TestInfrastructureFailuresAreInconclusive is the other half of the same rule. None of these say
// anything about whether the backup is restorable, and reporting them as failures would train an
// operator to ignore the one alert that matters.
func TestInfrastructureFailuresAreInconclusive(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(h *harness)
	}{
		{
			name: "the sandbox never became ready",
			arrange: func(h *harness) {
				h.sandboxes.provisionErr = errors.New("sandbox did not become ready")
			},
		},
		{
			name: "the plugin's host has no restore tool",
			arrange: func(h *harness) {
				h.plugin.restoreError = &fwv1.PluginError{
					Code:    fwv1.ErrorCode_ERROR_CODE_TOOL_NOT_FOUND,
					Message: `required tool "pg_restore" was not found on PATH`,
				}
			},
		},
		{
			name: "the checks themselves could not run",
			arrange: func(h *harness) {
				h.plugin.verifyErr = errors.New("the plugin went away")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := testTenantCtx()
			backupID := seedBackup(t, ctx, h, nil)
			tc.arrange(h)

			verificationID, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
			if err != nil {
				t.Fatalf("RunVerification: %v", err)
			}

			v := h.waitForVerification(t, verificationID)
			if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
				t.Fatalf("status = %v, want INCONCLUSIVE", v.GetStatus())
			}
			if v.GetErrorMessage() == "" {
				t.Error("no reason was recorded; an operator cannot tell what to fix")
			}
			h.sandboxes.assertNoneLeaked(t)
		})
	}
}

// TestAManifestlessBackupIsInconclusiveWithoutASandbox is the trap the brief names. Comparing zero
// objects to zero objects succeeds trivially, so a backup with no manifest must never reach a
// sandbox at all.
func TestAManifestlessBackupIsInconclusiveWithoutASandbox(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	backupID := seedBackup(t, ctx, h, func(b *seededBackup) { b.manifest = "{}" })

	verificationID, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
	if err != nil {
		t.Fatalf("RunVerification: %v", err)
	}

	v := h.waitForVerification(t, verificationID)
	if v.GetStatus() != fwv1.VerificationStatus_VERIFICATION_STATUS_INCONCLUSIVE {
		t.Fatalf("status = %v, want INCONCLUSIVE for a backup with no manifest", v.GetStatus())
	}
	if h.sandboxes.provisioned() != 0 {
		t.Error("a sandbox was provisioned for a comparison that could never mean anything")
	}
}

// TestVerificationRefusesABackupItCannotProve covers the requests that are rejected outright, before
// any row exists.
func TestVerificationRefusesABackupItCannotProve(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	tests := []struct {
		name    string
		seed    func(*seededBackup)
		wantErr error
	}{
		{
			name:    "a failed backup has no artifact to restore",
			seed:    func(b *seededBackup) { b.state = "failed"; b.objectKey = "" },
			wantErr: ErrNotVerifiable,
		},
		{
			// Without one there is no way to tell a corrupted artifact from a bad restore.
			name:    "a backup with no checksum",
			seed:    func(b *seededBackup) { b.checksum = "" },
			wantErr: ErrNotVerifiable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			backupID := seedBackup(t, ctx, h, tc.seed)
			_, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}

	if _, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: "not-a-uuid"}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("error = %v, want ErrInvalidArgument", err)
	}
	if _, err := h.svc.GetVerification(ctx, "3f2504e0-4f89-11d3-9a0c-0305e82c3301"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestVerifyOnCompletionChainsAVerification proves the product's core loop written out by hand: a
// backup that succeeds is followed by its own proof, as a separate job with its own row.
func TestVerifyOnCompletionChainsAVerification(t *testing.T) {
	h := newHarness(t)
	ctx := testTenantCtx()

	h.plugin.payload = []byte(strings.Repeat("verified-artifact-bytes ", 1024))
	h.plugin.manifest = &fwv1.SourceManifest{
		Entries: []*fwv1.ManifestEntry{
			{Database: "app", ObjectName: "public.orders", RecordCount: 120},
		},
		TotalObjects: 1,
		TotalRecords: 120,
	}
	h.plugin.verifyResult = &fwv1.VerifyRestoreResult{
		Status: fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED,
		Checks: []*fwv1.CheckResult{{
			Check: fwv1.VerificationCheck_VERIFICATION_CHECK_RECORD_COUNTS, Passed: true,
		}},
	}

	backupID, _, err := h.svc.RunBackup(ctx, RunBackupInput{
		InstanceID:         h.instance,
		VerifyOnCompletion: true,
		TriggeredManually:  true,
	})
	if err != nil {
		t.Fatalf("RunBackup: %v", err)
	}

	b := h.waitForState(t, backupID)
	if b.GetState() != fwv1.BackupState_BACKUP_STATE_SUCCEEDED {
		t.Fatalf("backup state = %v: %s", b.GetState(), b.GetErrorMessage())
	}

	// The two-part status: the backup and its proof are separate facts, and GetBackup carries both.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		got, _, err := h.svc.GetBackup(ctx, backupID)
		if err != nil {
			t.Fatalf("GetBackup: %v", err)
		}
		if got.GetVerification().GetStatus() == fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
			h.sandboxes.assertNoneLeaked(t)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the backup succeeded but no verification of it was ever recorded")
}

// -----------------------------------------------------------------------------------------------
// Stubs
// -----------------------------------------------------------------------------------------------

// sdkArtifactCorrupt mirrors what a plugin sends when it blames the artifact itself, without
// importing the plugin SDK's constructor into a core test.
func sdkArtifactCorrupt(message string) *fwv1.PluginError {
	return &fwv1.PluginError{
		Code:    fwv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		Message: message,
		Details: map[string]string{"artifact": "corrupt"},
	}
}

// stubSandboxes records every sandbox it hands out and every one destroyed, so a test can assert
// that nothing leaked — which is the property the whole sandbox design exists to guarantee.
type stubSandboxes struct {
	mu           sync.Mutex
	specs        []sandbox.Spec
	live         int
	provisionErr error
}

func (s *stubSandboxes) Provision(_ context.Context, spec sandbox.Spec) (sandbox.Sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.provisionErr != nil {
		return nil, s.provisionErr
	}
	s.specs = append(s.specs, spec)
	s.live++
	return &stubSandbox{provider: s, id: fmt.Sprintf("sandbox-%d", len(s.specs))}, nil
}

func (s *stubSandboxes) Sweep(context.Context) (int, error) { return 0, nil }
func (s *stubSandboxes) HealthCheck(context.Context) error  { return nil }
func (s *stubSandboxes) Close() error                       { return nil }

func (s *stubSandboxes) provisioned() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.specs)
}

func (s *stubSandboxes) lastSpec() sandbox.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.specs) == 0 {
		return sandbox.Spec{}
	}
	return s.specs[len(s.specs)-1]
}

func (s *stubSandboxes) assertNoneLeaked(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live != 0 {
		t.Errorf("%d sandboxes were never destroyed", s.live)
	}
}

type stubSandbox struct {
	provider *stubSandboxes
	id       string
	once     sync.Once
}

func (s *stubSandbox) ID() string { return s.id }

func (s *stubSandbox) Credentials() *fwv1.Credentials {
	return &fwv1.Credentials{
		Host: "127.0.0.1", Port: 55432, Username: "fleetward",
		Password: "generated-for-this-sandbox", Database: "fleetward_sandbox",
	}
}

func (s *stubSandbox) Destroy(context.Context) error {
	s.once.Do(func() {
		s.provider.mu.Lock()
		s.provider.live--
		s.provider.mu.Unlock()
	})
	return nil
}

// --- The plugin's restore and verify halves ------------------------------------------------------

func (s *stubEngine) Restore(ctx context.Context, in *fwv1.RestoreRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[fwv1.RestoreProgress], error) {
	s.mu.Lock()
	s.restore = in
	restoreErr := s.restoreError
	s.mu.Unlock()

	messages := []*fwv1.RestoreProgress{{
		RestoreId: in.GetRestoreId(),
		Phase:     fwv1.JobPhase_JOB_PHASE_TRANSFERRING,
		Message:   "downloading",
	}}
	if restoreErr != nil {
		messages = append(messages, &fwv1.RestoreProgress{
			RestoreId: in.GetRestoreId(),
			Phase:     fwv1.JobPhase_JOB_PHASE_FAILED,
			Error:     restoreErr,
		})
	} else {
		messages = append(messages, &fwv1.RestoreProgress{
			RestoreId: in.GetRestoreId(),
			Phase:     fwv1.JobPhase_JOB_PHASE_COMPLETED,
			Result:    &fwv1.RestoreResult{RestoredDatabases: []string{"fleetward_sandbox"}},
		})
	}

	return &stubRestoreStream{ctx: ctx, messages: messages}, nil
}

func (s *stubEngine) VerifyRestore(_ context.Context, in *fwv1.VerifyRestoreRequest, _ ...grpc.CallOption) (*fwv1.VerifyRestoreResult, error) {
	s.mu.Lock()
	s.verify = in
	result, err := s.verifyResult, s.verifyErr
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if result == nil {
		result = &fwv1.VerifyRestoreResult{Status: fwv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED}
	}
	return result, nil
}

func (s *stubEngine) lastRestore() *fwv1.RestoreRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restore
}

func (s *stubEngine) lastVerify() *fwv1.VerifyRestoreRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verify
}

type stubRestoreStream struct {
	ctx      context.Context
	messages []*fwv1.RestoreProgress
	next     int
}

func (s *stubRestoreStream) Recv() (*fwv1.RestoreProgress, error) {
	if s.next >= len(s.messages) {
		return nil, io.EOF
	}
	msg := s.messages[s.next]
	s.next++
	return msg, nil
}

func (s *stubRestoreStream) Header() (metadata.MD, error) { return nil, nil }
func (s *stubRestoreStream) Trailer() metadata.MD         { return nil }
func (s *stubRestoreStream) CloseSend() error             { return nil }
func (s *stubRestoreStream) Context() context.Context     { return s.ctx }
func (s *stubRestoreStream) SendMsg(any) error            { return nil }
func (s *stubRestoreStream) RecvMsg(any) error            { return io.EOF }
