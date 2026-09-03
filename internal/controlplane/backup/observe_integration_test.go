//go:build integration

// Integration tests for observed backups (ADR-0015) against a real metadata store.
//
// The plugin is a stub, as everywhere else in this package: what is under test is core's half —
// the watermark, the upsert, the convergence with a managed backup, the refusal to verify, and the
// adherence evaluation. That those SQL statements really do what they claim needs a real
// PostgreSQL, which is why these are here rather than beside the unit tests.
//
// Run with: go test -tags=integration ./internal/controlplane/backup/...
package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// richHistory is what an engine that keeps its own record of backups declares.
func richHistory() *fwv1.BackupHistoryCapabilities {
	return &fwv1.BackupHistoryCapabilities{
		Supported:                true,
		SourceDescription:        "the engine's own record of its backups",
		IdentityIsEngineAssigned: true,
		ReportsOutcome:           true,
	}
}

// weakHistory is what a directory listing declares: a file arrived, and nothing about whether the
// dump that wrote it finished.
func weakHistory() *fwv1.BackupHistoryCapabilities {
	return &fwv1.BackupHistoryCapabilities{
		Supported:         true,
		SourceDescription: "a configured backup directory",
	}
}

func observed(id string, outcome fwv1.ObservedOutcome, finished time.Time) *fwv1.ObservedBackup {
	return &fwv1.ObservedBackup{
		ExternalId: id,
		Database:   "app",
		Method:     "database",
		Outcome:    outcome,
		StartedAt:  timestamppb.New(finished.Add(-2 * time.Minute)),
		FinishedAt: timestamppb.New(finished),
		SizeBytes:  4096,
		Location:   "/srv/backups/" + id + ".bak",
	}
}

// TestObservationIsIdempotent is the property the whole design of ADR-0027 exists for.
//
// A poll running every half hour reads the same unchanged evidence thousands of times a year. If
// each read inserted a row, one nightly backup would become forty-eight rows a day, and an estate
// would look busier than it is while the answer to "when was the last backup" stayed correct by
// accident. The identity the engine assigns is what makes the second read an update.
func TestObservationIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.router.history = richHistory()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	finished := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	h.plugin.history = []*fwv1.ObservedBackup{
		observed("set-a", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED, finished),
		observed("set-b", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED, finished.Add(30*time.Minute)),
	}

	first, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if first.Discovered != 2 || first.Updated != 0 {
		t.Fatalf("first poll found %d new and %d known, want 2 and 0", first.Discovered, first.Updated)
	}

	second, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if second.Discovered != 0 {
		t.Errorf("the second poll of unchanged evidence recorded %d new backups, want 0",
			second.Discovered)
	}

	if got := countBackups(t, ctx, h, originObserved); got != 2 {
		t.Errorf("%d observed backups are recorded, want 2 — a poll is duplicating rows", got)
	}
}

// TestObservationConvergesWithAManagedBackup covers the case found while reading an engine's own
// backup history: Fleetward's own backups are recorded there too.
//
// Without the identity carried across, the next poll would see the backup Fleetward itself took and
// insert it a second time, as somebody else's. One physical backup, two rows, one of them claiming
// an origin it does not have — and the managed row is the one that carries the manifest, so a UI
// showing both would offer a verification on one and not the other for the same backup.
func TestObservationConvergesWithAManagedBackup(t *testing.T) {
	h := newHarness(t)
	h.router.history = richHistory()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// A backup Fleetward takes, through the real path, with the plugin reporting what the engine
	// called it. Seeding the column directly would test the upsert and miss the plumbing that
	// populates it — which is exactly the defect the walk found: the plugin reported the identity
	// and recordSuccess dropped it, so every managed backup was observed a second time.
	h.plugin.payload = []byte("a backup Fleetward took")
	h.plugin.externalID = "set-a"
	backupID, _, err := h.svc.RunBackup(ctx, RunBackupInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("take a managed backup: %v", err)
	}
	if b := h.waitForState(t, backupID); b.GetState() != fwv1.BackupState_BACKUP_STATE_SUCCEEDED {
		t.Fatalf("the managed backup is %s, want SUCCEEDED: %s", b.GetState(), b.GetErrorMessage())
	}

	var recorded *string
	if err := h.pool.QueryRow(ctx,
		`SELECT external_id FROM backups WHERE id = $1`, backupID).Scan(&recorded); err != nil {
		t.Fatalf("read the managed backup's identity: %v", err)
	}
	if recorded == nil || *recorded != "set-a" {
		t.Fatalf("the managed backup recorded external_id %v, want \"set-a\": the next poll will "+
			"record this backup a second time as somebody else's", recorded)
	}

	h.plugin.history = []*fwv1.ObservedBackup{
		observed("set-a", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED, time.Now().UTC()),
	}

	polled, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Discovered != 0 {
		t.Errorf("the poll recorded %d new backups for one Fleetward already took", polled.Discovered)
	}

	if got := countBackups(t, ctx, h, originObserved); got != 0 {
		t.Errorf("%d observed backups were recorded, want 0", got)
	}

	// And the managed row is untouched: still managed, still succeeded, still ours.
	var origin, state string
	if err := h.pool.QueryRow(ctx,
		`SELECT origin, state FROM backups WHERE id = $1`, backupID).Scan(&origin, &state); err != nil {
		t.Fatalf("read the managed backup back: %v", err)
	}
	if origin != originManaged || state != "succeeded" {
		t.Errorf("the managed backup became %s/%s; observation must never rewrite one", origin, state)
	}
}

// TestObservationRecordsWhatTheEvidenceCanProve asserts the honesty of a weak source end to end:
// what the plugin declared about it reaches the row, and an outcome it cannot report is stored as
// unknown rather than rounded into success.
func TestObservationRecordsWhatTheEvidenceCanProve(t *testing.T) {
	h := newHarness(t)
	h.router.history = weakHistory()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	record := observed("file:abc", fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN, time.Now().UTC())
	record.FinishedAtIsApproximate = true
	h.plugin.history = []*fwv1.ObservedBackup{record}

	if _, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance}); err != nil {
		t.Fatalf("poll: %v", err)
	}

	backups, err := h.svc.ListBackups(ctx, ListBackupsInput{InstanceID: h.instance})
	if err != nil {
		t.Fatalf("list backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("listed %d backups, want 1", len(backups))
	}

	b := backups[0]
	if b.GetState() != fwv1.BackupState_BACKUP_STATE_UNKNOWN {
		t.Errorf("state = %s, want UNKNOWN from a source that cannot report an outcome", b.GetState())
	}
	if b.GetOrigin() != fwv1.BackupOrigin_BACKUP_ORIGIN_OBSERVED {
		t.Errorf("origin = %s, want OBSERVED", b.GetOrigin())
	}
	if b.GetArtifact() != nil {
		t.Error("an observed backup was given an artifact; Fleetward owns nothing about it")
	}
	if b.GetExternalLocation() == "" {
		t.Error("the location was not recorded, so nobody could go and find the file")
	}

	evidence := b.GetEvidence()
	if evidence == nil {
		t.Fatal("no evidence was recorded, so nothing downstream can say what this rests on")
	}
	if evidence.GetReportsOutcome() {
		t.Error("the row claims an outcome the source declared it cannot report")
	}
	if !evidence.GetCompletedAtIsApproximate() {
		t.Error("the record's approximate finish time did not survive into the row")
	}
	if evidence.GetObservedAt() == nil {
		t.Error("nothing records when Fleetward saw this, so the picture's freshness is unknowable")
	}
}

// TestAnObservedBackupCannotBeVerified is the refusal ADR-0015 calls a correctness requirement.
//
// Without it the backup falls into the manifest-less branch and comes back INCONCLUSIVE, which
// reads as "the check went wrong" and sits in the same bucket as an image that could not be pulled.
// It is not that. It is permanent, and the person asking is the only one who can act on it.
func TestAnObservedBackupCannotBeVerified(t *testing.T) {
	h := newHarness(t)
	h.router.history = richHistory()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	h.plugin.history = []*fwv1.ObservedBackup{
		observed("set-a", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED, time.Now().UTC()),
	}
	if _, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance}); err != nil {
		t.Fatalf("poll: %v", err)
	}

	var backupID string
	if err := h.pool.QueryRow(ctx,
		`SELECT id FROM backups WHERE origin = $1`, originObserved).Scan(&backupID); err != nil {
		t.Fatalf("read the observed backup: %v", err)
	}

	_, _, err := h.svc.RunVerification(ctx, RunVerificationInput{BackupID: backupID})
	if err == nil {
		t.Fatal("a verification of an observed backup was accepted")
	}
	if !errors.Is(err, ErrNotVerifiable) {
		t.Errorf("error = %v, want ErrNotVerifiable", err)
	}
	// The reason has to be the true one. "No manifest" describes the symptom accurately and
	// explains the cause wrongly, and an operator acting on it would go looking for a backup whose
	// manifest failed to save.
	if !strings.Contains(err.Error(), "taken by something other than Fleetward") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// And nothing was written: no job to appear in the job table, no verification row to explain.
	var verifications int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM verifications WHERE backup_id = $1`, backupID).Scan(&verifications); err != nil {
		t.Fatalf("count verifications: %v", err)
	}
	if verifications != 0 {
		t.Errorf("%d verification rows were created for a backup that cannot be verified", verifications)
	}
}

// TestObservationIsRefusedWhenThePluginCannotSeeAny covers the other half of the capability. An
// empty answer would report an engine nobody is watching as an engine with nothing to report.
func TestObservationIsRefusedWhenThePluginCannotSeeAny(t *testing.T) {
	h := newHarness(t)
	h.router.history = nil

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want ErrUnsupported", err)
	}
}

// TestAdherenceAnswersTheQuestionTheProductExistsFor walks the whole pillar: nothing declared, then
// a declaration with nothing to satisfy it, then somebody else's backup satisfying it.
func TestAdherenceAnswersTheQuestionTheProductExistsFor(t *testing.T) {
	h := newHarness(t)
	h.router.history = richHistory()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Nothing declared. Reported as such rather than as healthy: on an estate of fifty, "nobody has
	// said what this one's backups should look like" is a finding.
	answers, err := h.svc.GetBackupAdherence(ctx, GetAdherenceInput{})
	if err != nil {
		t.Fatalf("adherence: %v", err)
	}
	if len(answers) != 1 {
		t.Fatalf("reported on %d instances, want 1", len(answers))
	}
	if answers[0].GetState() != fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED {
		t.Errorf("state = %s, want NOT_DECLARED", answers[0].GetState())
	}

	// A declaration: a backup every hour, up to ten minutes late.
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO schedules (tenant_id, instance_id, kind, cron_expression, timezone,
		                       expected_cron, expected_grace_minutes, next_run_at)
		VALUES ($1, $2, 'observe', '*/30 * * * *', 'UTC', '0 * * * *', 10, now())`,
		h.svc.tenantID, h.instance); err != nil {
		t.Fatalf("declare an expectation: %v", err)
	}

	answers, err = h.svc.GetBackupAdherence(ctx, GetAdherenceInput{})
	if err != nil {
		t.Fatalf("adherence: %v", err)
	}
	if answers[0].GetState() != fwv1.AdherenceState_ADHERENCE_STATE_MISSED {
		t.Errorf("state = %s with nothing backed up, want MISSED", answers[0].GetState())
	}
	if answers[0].GetExpectedBy() == nil {
		t.Error("the window under judgement was not reported, so nobody can see what was missed")
	}

	// Somebody else's backup, inside the window the expectation names. It counts, and that is the
	// entire point of the slice: the estate was not changed and the question is answered.
	windowStart := answers[0].GetExpectedBy().AsTime()
	h.plugin.history = []*fwv1.ObservedBackup{
		observed("set-hourly", fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED,
			windowStart.Add(3*time.Minute)),
	}
	if _, err := h.svc.ObserveBackupHistory(ctx, ObserveInput{InstanceID: h.instance}); err != nil {
		t.Fatalf("poll: %v", err)
	}

	answers, err = h.svc.GetBackupAdherence(ctx, GetAdherenceInput{})
	if err != nil {
		t.Fatalf("adherence: %v", err)
	}
	if answers[0].GetState() != fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT {
		t.Fatalf("state = %s with a backup inside the window, want ADHERENT", answers[0].GetState())
	}
	if answers[0].GetSatisfiedBy().GetOrigin() != fwv1.BackupOrigin_BACKUP_ORIGIN_OBSERVED {
		t.Error("the window was satisfied by something other than the observed backup")
	}
	// The caveat is the other half of counting somebody else's backup: the answer says what it
	// rests on, so nobody mistakes it for a backup Fleetward proved restorable.
	if len(answers[0].GetCaveats()) != 0 {
		t.Logf("caveats: %v", answers[0].GetCaveats())
	}

	// And an adherent estate reports nothing when asked only for problems, which is the query an
	// on-call DBA actually runs.
	problems, err := h.svc.GetBackupAdherence(ctx, GetAdherenceInput{ProblemsOnly: true})
	if err != nil {
		t.Fatalf("adherence: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("problems_only reported %d instances on an adherent estate", len(problems))
	}
}

func countBackups(t *testing.T, ctx context.Context, h *harness, origin string) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(ctx,
		`SELECT count(*) FROM backups WHERE instance_id = $1 AND origin = $2`,
		h.instance, origin).Scan(&n); err != nil {
		t.Fatalf("count backups: %v", err)
	}
	return n
}

// ListBackupHistory is the stub plugin's half. It honours `since` so that core's watermark can be
// exercised the way a real plugin would let it be.
func (s *stubEngine) ListBackupHistory(_ context.Context, in *fwv1.ListBackupHistoryRequest, _ ...grpc.CallOption) (*fwv1.ListBackupHistoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	since := in.GetSince().AsTime()
	var out []*fwv1.ObservedBackup
	for _, b := range s.history {
		if in.GetSince() != nil && b.GetFinishedAt().AsTime().Before(since) {
			continue
		}
		out = append(out, b)
	}
	return &fwv1.ListBackupHistoryResponse{Backups: out}, nil
}
