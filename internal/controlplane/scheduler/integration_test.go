//go:build integration

// Integration tests for the scheduler against a real metadata store.
//
// What is under test is the lease protocol, and it can only be tested against a real PostgreSQL:
// every guarantee in it is a property of one SQL statement — that `UPDATE ... WHERE id = (SELECT
// ... FOR UPDATE SKIP LOCKED)` claims exactly one row under contention, that a heartbeat predicated
// on lease_owner matches nothing once the owner changed, that a partial unique index raises 23505
// rather than inserting. A fake would be asserting that the test's own model is self-consistent.
//
// The backup service is a stub on purpose. The work a job does is covered elsewhere, against real
// engines; what belongs here is who is allowed to do it, and when.
//
// Requires Docker and no pre-installed PostgreSQL.
//
// Run with: go test -tags=integration ./internal/controlplane/scheduler/...
package scheduler

import (
	"context"
	"errors"
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

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

const (
	metaImage    = "postgres:16-alpine"
	metaDB       = "fleetward"
	metaUser     = "fleetward"
	metaPass     = "fleetward-integration"
	startTimeout = 3 * time.Minute
)

// stubRunner stands in for the backup service. It records what it was asked to do, can be made to
// block until released, and reports whether its context was cancelled — which is how the lease-loss
// test observes that the work actually stopped.
type stubRunner struct {
	mu sync.Mutex

	backupIDs []string
	verified  []string
	observed  []string
	probed    []string
	sweeps    int

	// block, when non-nil, holds RunBackupJob until it is closed or the context is cancelled.
	block chan struct{}
	// ctxErr records how a blocked run ended.
	ctxErr error
	// started is closed the first time RunBackupJob is entered.
	started chan struct{}
	// unwind is how long a cancelled run takes to finish tidying up, standing in for the detached
	// write that records a cancelled backup as failed.
	unwind time.Duration
	// didUnwind reports that a cancelled run got all the way to its return statement.
	didUnwind bool

	err error
}

func newStubRunner() *stubRunner {
	return &stubRunner{started: make(chan struct{})}
}

func (r *stubRunner) RunBackupJob(ctx context.Context, in BackupJob) (string, error) {
	r.mu.Lock()
	r.backupIDs = append(r.backupIDs, in.JobID)
	block, failWith := r.block, r.err
	if len(r.backupIDs) == 1 {
		close(r.started)
	}
	r.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			r.mu.Lock()
			r.ctxErr = ctx.Err()
			unwind := r.unwind
			r.mu.Unlock()

			time.Sleep(unwind)

			r.mu.Lock()
			r.didUnwind = true
			r.mu.Unlock()
			return "", ctx.Err()
		}
	}
	if failWith != nil {
		return "", failWith
	}
	return "00000000-0000-4000-8000-00000000beef", nil
}

func (r *stubRunner) RunVerificationJob(_ context.Context, in VerificationJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verified = append(r.verified, in.BackupID)
	return nil
}

func (r *stubRunner) RunObservationJob(_ context.Context, in ObservationJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observed = append(r.observed, in.InstanceID)
	return r.err
}

func (r *stubRunner) observations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.observed...)
}

func (r *stubRunner) RunDiscoveryJob(_ context.Context, in DiscoveryJob) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probed = append(r.probed, in.InstanceID)
	return r.err
}

// SweepRetention is the one Runner method that is not a job. The stub records that it was called
// and does nothing, which is all the scheduler's own tests need: what a sweep actually deletes is
// the backup service's business and is tested there, against a real object store.
func (r *stubRunner) SweepRetention(context.Context) (RetentionOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweeps++
	return RetentionOutcome{}, nil
}

func (r *stubRunner) probes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.probed...)
}

func (r *stubRunner) jobsRun() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.backupIDs...)
}

func (r *stubRunner) cancelled() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctxErr
}

func (r *stubRunner) unwound() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.didUnwind
}

// harness is one test's isolated world: a real metadata store with the real schema.
type harness struct {
	pool       *pgxpool.Pool
	log        *slog.Logger
	instanceID string
	tenantID   string
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

	h := &harness{pool: db.Pool(), log: log, tenantID: metadb.DefaultTenantID}
	h.instanceID = h.newInstance(t, "prod-1")
	return h
}

// newInstance inserts the minimum an instance needs for a job to reference it. The inventory
// service is not involved: this suite is about the scheduler, and going through inventory would
// drag a secrets provider and a plugin router into a test about SQL.
func (h *harness) newInstance(t *testing.T, name string) string {
	t.Helper()

	var envID string
	err := h.pool.QueryRow(context.Background(), `
		INSERT INTO environments (tenant_id, name) VALUES ($1, $2)
		ON CONFLICT (tenant_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id`, h.tenantID, "production").Scan(&envID)
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	var instanceID string
	err = h.pool.QueryRow(context.Background(), `
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, $3, 'testengine', 'db.example.internal', 5432)
		RETURNING id`, h.tenantID, envID, name).Scan(&instanceID)
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return instanceID
}

// newJob inserts a pending job, the way materialize would.
func (h *harness) newJob(t *testing.T, kind string) string {
	t.Helper()

	payload, err := jobPayload{VerifyPolicy: verifyManual}.encode()
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}

	var jobID string
	if err := h.pool.QueryRow(context.Background(), `
		INSERT INTO jobs (tenant_id, instance_id, kind, state, payload, scheduled_for)
		VALUES ($1, $2, $3, 'pending', $4, now())
		RETURNING id`, h.tenantID, h.instanceID, kind, payload).Scan(&jobID); err != nil {
		t.Fatalf("create job: %v", err)
	}
	return jobID
}

func (h *harness) jobRow(t *testing.T, jobID string) (state, owner, errorMessage string) {
	t.Helper()
	if err := h.pool.QueryRow(context.Background(), `
		SELECT state, COALESCE(lease_owner, ''), error_message FROM jobs WHERE id = $1`, jobID).
		Scan(&state, &owner, &errorMessage); err != nil {
		t.Fatalf("read job %s: %v", jobID, err)
	}
	return state, owner, errorMessage
}

func (h *harness) attempts(t *testing.T, jobID string) int32 {
	t.Helper()
	var n int32
	if err := h.pool.QueryRow(context.Background(),
		`SELECT attempts FROM jobs WHERE id = $1`, jobID).Scan(&n); err != nil {
		t.Fatalf("read attempts for job %s: %v", jobID, err)
	}
	return n
}

func (h *harness) scheduler(runner Runner, cfg config.SchedulerConfig) *Scheduler {
	// Retention is off in these tests. What they exercise is claiming, heartbeating, losing a lease
	// and reaping; a sweep firing on the first tick of every one of them would be unrelated work
	// deleting unrelated rows. The sweep has its own tests, against a real object store.
	return New(h.pool, runner, cfg, config.RetentionConfig{}, h.log)
}

// TestClaimTakesAJobExactlyOnce is the central guarantee. Two runners issue the same statement
// against the same pending job; one gets it and the other gets nothing.
//
// It is a single statement rather than SELECT-then-UPDATE precisely so that this holds without a
// transaction, and so that there is no window in which a job is claimed in the database but
// unrecorded in the process that claimed it.
func TestClaimTakesAJobExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jobID := h.newJob(t, kindBackup)

	ownerA, ownerB := newOwnerID(), newOwnerID()

	first, err := claim(ctx, h.pool, ownerA, time.Minute)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first == nil || first.ID != jobID {
		t.Fatalf("first claim = %v; want job %s", first, jobID)
	}

	second, err := claim(ctx, h.pool, ownerB, time.Minute)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second != nil {
		t.Fatalf("second runner also claimed job %s; at-most-once is broken", second.ID)
	}

	state, owner, _ := h.jobRow(t, jobID)
	if state != "running" || owner != ownerA {
		t.Fatalf("job is %s owned by %q; want running owned by %q", state, owner, ownerA)
	}

	// attempts counts starts, and the claim is the start. It is what `job list` shows, so a job run
	// once that reports two looks to an operator like a retry that never happened — which is
	// exactly what it did report until the closing UPDATEs stopped incrementing it too.
	if n := h.attempts(t, jobID); n != 1 {
		t.Fatalf("attempts = %d after one claim; want 1", n)
	}
}

// TestClaimIsRaceFreeUnderContention runs many claimers at once against a handful of jobs. Every
// job must be claimed by exactly one, and no claimer may receive a job twice.
func TestClaimIsRaceFreeUnderContention(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// One job per instance, because idx_jobs_one_active_per_instance_kind allows exactly that.
	const jobCount = 8
	want := map[string]bool{}
	for i := range jobCount {
		h.instanceID = h.newInstance(t, "contended-"+string(rune('a'+i)))
		want[h.newJob(t, kindBackup)] = true
	}

	const claimers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}

	wg.Add(claimers)
	for range claimers {
		go func() {
			defer wg.Done()
			owner := newOwnerID()
			for {
				job, err := claim(ctx, h.pool, owner, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				got[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(got) != len(want) {
		t.Fatalf("claimed %d distinct jobs; want %d", len(got), len(want))
	}
	for id, times := range got {
		if !want[id] {
			t.Fatalf("claimed a job that was not pending: %s", id)
		}
		if times != 1 {
			t.Fatalf("job %s was claimed %d times; at-most-once is broken", id, times)
		}
	}
}

// TestHeartbeatReportsALostLease is the subtle case this slice exists to get right.
//
// Zero rows affected is not a database error. It means the lease is gone, another process has
// already written this job's outcome, and the runner still holding it is a ghost.
func TestHeartbeatReportsALostLease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jobID := h.newJob(t, kindBackup)

	owner := newOwnerID()
	if _, err := claim(ctx, h.pool, owner, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if err := renew(ctx, h.pool, jobID, owner, time.Minute); err != nil {
		t.Fatalf("renewing a lease we hold: %v", err)
	}

	if err := renew(ctx, h.pool, jobID, newOwnerID(), time.Minute); !errors.Is(err, errLeaseLost) {
		t.Fatalf("renewing someone else's lease = %v; want errLeaseLost", err)
	}

	// And once the reaper has closed the job, even the rightful owner has lost it: the state
	// predicate is what stops a runner writing over a verdict already recorded.
	if _, err := h.pool.Exec(ctx,
		`UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, jobID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	if _, err := reap(ctx, h.pool); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if err := renew(ctx, h.pool, jobID, owner, time.Minute); !errors.Is(err, errLeaseLost) {
		t.Fatalf("renewing after the reaper = %v; want errLeaseLost", err)
	}
}

// TestReapClosesAbandonedWork covers the control plane killed mid-backup: the job is failed with a
// reason, the backup row it orphaned is failed too, and — deliberately — the job is not re-run
// (ADR-0025).
func TestReapClosesAbandonedWork(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jobID := h.newJob(t, kindBackup)

	owner := newOwnerID()
	if _, err := claim(ctx, h.pool, owner, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}

	var backupID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, job_id, method_id, state, started_at)
		VALUES ($1, $2, $3, 'pg_dump', 'running', now())
		RETURNING id`, h.tenantID, h.instanceID, jobID).Scan(&backupID); err != nil {
		t.Fatalf("create backup row: %v", err)
	}

	// Nothing to reap while the lease is live.
	if n, err := reap(ctx, h.pool); err != nil || n != 0 {
		t.Fatalf("reap with a live lease = %d, %v; want 0, nil", n, err)
	}

	if _, err := h.pool.Exec(ctx,
		`UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, jobID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}

	n, err := reap(ctx, h.pool)
	if err != nil || n != 1 {
		t.Fatalf("reap = %d, %v; want 1, nil", n, err)
	}

	state, ownerAfter, message := h.jobRow(t, jobID)
	if state != "failed" {
		t.Fatalf("reaped job is %s; want failed — a job stuck at running is the debt this closes", state)
	}
	if ownerAfter != "" {
		t.Fatalf("reaped job still names owner %q", ownerAfter)
	}
	if !strings.Contains(message, "stopped reporting") {
		t.Fatalf("reaped job says %q; an operator needs to be told what happened", message)
	}

	var backupState, backupError string
	if err := h.pool.QueryRow(ctx,
		`SELECT state, error_message FROM backups WHERE id = $1`, backupID).
		Scan(&backupState, &backupError); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if backupState != "failed" {
		t.Fatalf("the orphaned backup is %s; a backup stuck at running for a month is the bug", backupState)
	}
	if !strings.Contains(backupError, "stopped reporting") {
		t.Fatalf("the orphaned backup says %q", backupError)
	}

	if n := h.attempts(t, jobID); n != 1 {
		t.Fatalf("attempts = %d on a job that was started once and then reaped; want 1", n)
	}

	// The decision that separates this design from "make it claimable again": nothing re-runs it.
	again, err := claim(ctx, h.pool, newOwnerID(), time.Minute)
	if err != nil {
		t.Fatalf("claim after reap: %v", err)
	}
	if again != nil {
		t.Fatalf("a reaped job was claimed again (%s); ADR-0025 says the next schedule creates a new one", again.ID)
	}
}

// TestReapReportsAnInterruptedVerificationAsInconclusive keeps ADR-0022 intact under a crash. A
// control plane that was killed is evidence about the control plane, never about the artifact —
// and FAILED is the product's one critical alert.
func TestReapReportsAnInterruptedVerificationAsInconclusive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	backupJobID := h.newJob(t, kindBackup)
	var backupID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, job_id, method_id, state, completed_at)
		VALUES ($1, $2, $3, 'pg_dump', 'succeeded', now())
		RETURNING id`, h.tenantID, h.instanceID, backupJobID).Scan(&backupID); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE jobs SET state = 'succeeded', finished_at = now() WHERE id = $1`, backupJobID); err != nil {
		t.Fatalf("close the backup job: %v", err)
	}

	verifyJobID := h.newJob(t, kindVerify)
	var verificationID string
	if err := h.pool.QueryRow(ctx, `
		INSERT INTO verifications (tenant_id, backup_id, job_id, status, started_at)
		VALUES ($1, $2, $3, 'running', now())
		RETURNING id`, h.tenantID, backupID, verifyJobID).Scan(&verificationID); err != nil {
		t.Fatalf("create verification: %v", err)
	}

	if _, err := claim(ctx, h.pool, newOwnerID(), time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE jobs SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, verifyJobID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	if _, err := reap(ctx, h.pool); err != nil {
		t.Fatalf("reap: %v", err)
	}

	var status string
	if err := h.pool.QueryRow(ctx,
		`SELECT status FROM verifications WHERE id = $1`, verificationID).Scan(&status); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	if status != "inconclusive" {
		t.Fatalf("an interrupted verification is %q; ADR-0022 reserves 'failed' for evidence about the artifact", status)
	}
}

// TestMaterializeCreatesOneJobPerDueSchedule walks the whole path a schedule takes: due, advanced,
// turned into a job, and not turned into a second one on the next tick.
func TestMaterializeCreatesOneJobPerDueSchedule(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	svc := NewService(h.pool, h.log)
	created, err := svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:     h.instanceID,
		CronExpression: "* * * * *",
		Timezone:       "Europe/Bucharest",
		VerifyPolicy:   verifyAlways,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if created.GetNextRunAt() == nil {
		t.Fatal("a new schedule has no next run; the tick loop would never see it")
	}

	// Make it due.
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due: %v", err)
	}

	s := h.scheduler(newStubRunner(), config.SchedulerConfig{Enabled: true, LeaseTTL: time.Minute})
	n, err := s.materialize(ctx)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if n != 1 {
		t.Fatalf("materialize created %d jobs; want 1", n)
	}

	// The second tick must create nothing: next_run_at has advanced, and even if it had not, the
	// partial unique index would refuse a second pending backup for this instance.
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due again: %v", err)
	}
	if n, err := s.materialize(ctx); err != nil || n != 0 {
		t.Fatalf("second materialize = %d, %v; a run that is still active must be skipped, not duplicated", n, err)
	}

	jobs, err := svc.ListJobs(ctx, ListJobsFilter{InstanceID: h.instanceID})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("the instance has %d jobs; want exactly 1", len(jobs))
	}
	if jobs[0].GetScheduleId() != created.GetId() {
		t.Fatalf("job names schedule %q; want %q", jobs[0].GetScheduleId(), created.GetId())
	}
}

// TestMaterializeIsSafeWithTwoSchedulers is the multi-replica case. Two schedulers tick against the
// same due schedule at the same time; the compare-and-swap on next_run_at means exactly one job
// exists afterwards.
func TestMaterializeIsSafeWithTwoSchedulers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	svc := NewService(h.pool, h.log)
	created, err := svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:     h.instanceID,
		CronExpression: "* * * * *",
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due: %v", err)
	}

	cfg := config.SchedulerConfig{Enabled: true, LeaseTTL: time.Minute}
	a, b := h.scheduler(newStubRunner(), cfg), h.scheduler(newStubRunner(), cfg)

	var wg sync.WaitGroup
	total := make(chan int, 2)
	wg.Add(2)
	for _, s := range []*Scheduler{a, b} {
		go func() {
			defer wg.Done()
			n, err := s.materialize(ctx)
			if err != nil {
				t.Errorf("materialize: %v", err)
			}
			total <- n
		}()
	}
	wg.Wait()
	close(total)

	sum := 0
	for n := range total {
		sum += n
	}
	if sum != 1 {
		t.Fatalf("two schedulers created %d jobs from one due schedule; want 1", sum)
	}
}

// TestRunnerAbandonsAJobWhoseLeaseWasTaken is the end-to-end version of the subtle case: a job is
// claimed, its work blocks, the lease is stolen, and the runner must cancel its own work and leave
// the other process's verdict standing.
func TestRunnerAbandonsAJobWhoseLeaseWasTaken(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jobID := h.newJob(t, kindBackup)

	runner := newStubRunner()
	runner.block = make(chan struct{})
	defer close(runner.block)

	s := h.scheduler(runner, config.SchedulerConfig{
		Enabled:        true,
		LeaseTTL:       2 * time.Second,
		LeaseHeartbeat: 200 * time.Millisecond,
		PollInterval:   time.Second,
	})

	job, err := claim(ctx, h.pool, s.owner, s.cfg.LeaseTTL)
	if err != nil || job == nil {
		t.Fatalf("claim: %v (job %v)", err, job)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run(ctx, job)
	}()

	select {
	case <-runner.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never started the work")
	}

	// Another process takes the job: exactly what the reaper does after this lease expires.
	stolenBy := newOwnerID()
	if _, err := h.pool.Exec(ctx, `
		UPDATE jobs SET lease_owner = $2, state = 'failed',
		                error_message = 'closed by another process', finished_at = now()
		WHERE id = $1`, jobID, stolenBy); err != nil {
		t.Fatalf("steal the lease: %v", err)
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the runner did not stop after losing its lease; it is still holding a connection to a database")
	}

	if err := runner.cancelled(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the work ended with %v; losing a lease must cancel the work's own context", err)
	}

	state, owner, message := h.jobRow(t, jobID)
	if state != "failed" || owner != stolenBy || message != "closed by another process" {
		t.Fatalf("the ghost runner overwrote the verdict: state=%s owner=%q message=%q", state, owner, message)
	}
}

// TestSchedulerRunsAndQueuesVerification is the slice's headline, end to end and unattended: a
// schedule falls due, a backup job is created, claimed and run, and its verification is queued as
// its own row rather than chained in-process.
func TestSchedulerRunsAndQueuesVerification(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	svc := NewService(h.pool, h.log)
	created, err := svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:     h.instanceID,
		CronExpression: "* * * * *",
		VerifyPolicy:   verifyAlways,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due: %v", err)
	}

	runner := newStubRunner()
	s := h.scheduler(runner, config.SchedulerConfig{
		Enabled:           true,
		LeaseTTL:          time.Minute,
		LeaseHeartbeat:    10 * time.Second,
		PollInterval:      200 * time.Millisecond,
		MaxConcurrentJobs: 2,
	})

	s.Start(ctx)
	defer func() { _ = s.Close() }()

	deadline := time.After(60 * time.Second)
	for {
		jobs, err := svc.ListJobs(ctx, ListJobsFilter{InstanceID: h.instanceID})
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		verifySeen := false
		for _, j := range jobs {
			if j.GetKind().String() == "JOB_KIND_VERIFY" {
				verifySeen = true
			}
		}
		if verifySeen && len(runner.jobsRun()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no verification job appeared; jobs so far: %d, backups run: %v", len(jobs), runner.jobsRun())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// TestCloseCancelsInFlightWorkAndWaitsForItToUnwind pins what shutdown actually promises.
//
// It does not promise that a running backup finishes: a job legitimately takes hours, and a control
// plane that refused to stop until one did would never restart. It promises the same thing the
// backup service's own Close promises — the work is cancelled, and the process does not go away
// until every runner has unwound and recorded its outcome. A run killed without writing its row is
// exactly the orphan the reaper exists to clean up, and a clean shutdown should not create one.
//
// This is also why the ordering in the control plane's main is load-bearing. The scheduler drains
// first; the backup service, which is waiting on the same runs through its own WaitGroup, drains
// second. Reversed, the backup service would be waiting while the scheduler was still starting new
// runs.
func TestCloseCancelsInFlightWorkAndWaitsForItToUnwind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.newJob(t, kindBackup)

	runner := newStubRunner()
	runner.block = make(chan struct{})
	defer close(runner.block)
	// Long enough that a Close returning early is unambiguous rather than a scheduling artefact.
	runner.unwind = 750 * time.Millisecond

	s := h.scheduler(runner, config.SchedulerConfig{
		Enabled:           true,
		LeaseTTL:          time.Minute,
		LeaseHeartbeat:    30 * time.Second,
		PollInterval:      100 * time.Millisecond,
		MaxConcurrentJobs: 1,
	})
	s.Start(ctx)

	select {
	case <-runner.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the scheduler never claimed the pending job")
	}

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close never returned")
	}

	if !runner.unwound() {
		t.Fatal("Close returned before the runner had unwound; a run cut off mid-write leaves the orphan " +
			"row that the reaper exists to clean up")
	}
	if err := runner.cancelled(); !errors.Is(err, context.Canceled) {
		t.Fatalf("the work ended with %v; shutdown must cancel it rather than wait hours for it", err)
	}
}

// TestSchedulerRunsAndClosesAnObservation covers the asymmetry that a walk found and no unit test
// would have.
//
// A backup and a verification write their own job's terminal state, in the same transaction as the
// row that explains the outcome. An observation writes no such row — it updates the backups it
// found, and nothing that is about the job — so nothing else closes it. Left running, the job then
// trips idx_jobs_one_active_per_instance_kind and every later observation of that instance is
// skipped: the polling stops, silently, and the estate keeps reporting whatever it last saw.
func TestSchedulerRunsAndClosesAnObservation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	svc := NewService(h.pool, h.log)
	created, err := svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:           h.instanceID,
		Kind:                 kindObserve,
		CronExpression:       "* * * * *",
		ExpectedCron:         "0 2 * * *",
		ExpectedGraceMinutes: 120,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if created.GetExpectedCron() != "0 2 * * *" || created.GetExpectedGraceMinutes() != 120 {
		t.Fatalf("the declaration did not survive the round trip: %q ±%d",
			created.GetExpectedCron(), created.GetExpectedGraceMinutes())
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due: %v", err)
	}

	runner := newStubRunner()
	s := h.scheduler(runner, config.SchedulerConfig{
		Enabled:           true,
		LeaseTTL:          time.Minute,
		LeaseHeartbeat:    10 * time.Second,
		PollInterval:      200 * time.Millisecond,
		MaxConcurrentJobs: 2,
	})

	s.Start(ctx)
	defer func() { _ = s.Close() }()

	deadline := time.After(60 * time.Second)
	for {
		jobs, err := svc.ListJobs(ctx, ListJobsFilter{InstanceID: h.instanceID})
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		done := false
		for _, j := range jobs {
			if j.GetKind() == fwv1.JobKind_JOB_KIND_OBSERVE &&
				j.GetState() == fwv1.JobState_JOB_STATE_SUCCEEDED {
				done = true
			}
		}
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no observation job reached a terminal state; observations run: %v, jobs: %d",
				runner.observations(), len(jobs))
		case <-time.After(200 * time.Millisecond):
		}
	}

	if got := runner.observations(); len(got) == 0 || got[0] != h.instanceID {
		t.Errorf("the runner was asked to observe %v, want [%s]", got, h.instanceID)
	}
}

// TestSchedulerRunsAndClosesAHealthProbe is the observation test's twin, and it is here for the
// same reason: a discovery job writes no row that is about itself, so if the runner does not close
// it, idx_jobs_one_active_per_instance_kind blocks every later probe of that instance and the
// estate's health silently stops moving. The screen would keep rendering a green dot with a
// timestamp that never advances, which is worse than rendering nothing.
func TestSchedulerRunsAndClosesAHealthProbe(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	svc := NewService(h.pool, h.log)
	created, err := svc.CreateSchedule(ctx, CreateScheduleInput{
		InstanceID:     h.instanceID,
		Kind:           kindDiscovery,
		CronExpression: "* * * * *",
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`UPDATE schedules SET next_run_at = now() - interval '1 minute' WHERE id = $1`, created.GetId()); err != nil {
		t.Fatalf("make the schedule due: %v", err)
	}

	runner := newStubRunner()
	s := h.scheduler(runner, config.SchedulerConfig{
		Enabled:           true,
		LeaseTTL:          time.Minute,
		LeaseHeartbeat:    10 * time.Second,
		PollInterval:      200 * time.Millisecond,
		MaxConcurrentJobs: 2,
	})

	s.Start(ctx)
	defer func() { _ = s.Close() }()

	deadline := time.After(60 * time.Second)
	for {
		jobs, err := svc.ListJobs(ctx, ListJobsFilter{InstanceID: h.instanceID})
		if err != nil {
			t.Fatalf("ListJobs: %v", err)
		}
		done := false
		for _, j := range jobs {
			if j.GetKind() == fwv1.JobKind_JOB_KIND_DISCOVERY &&
				j.GetState() == fwv1.JobState_JOB_STATE_SUCCEEDED {
				done = true
			}
		}
		if done {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no discovery job reached a terminal state; probes run: %v, jobs: %d",
				runner.probes(), len(jobs))
		case <-time.After(200 * time.Millisecond):
		}
	}

	if got := runner.probes(); len(got) == 0 || got[0] != h.instanceID {
		t.Errorf("the runner was asked to probe %v, want [%s]", got, h.instanceID)
	}
}
