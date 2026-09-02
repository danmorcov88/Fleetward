package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

// Job kinds and verification policies, as the CHECK constraints spell them.
const (
	kindBackup = "backup"
	kindVerify = "verify"

	verifyAlways  = "always"
	verifySampled = "sampled"
	verifyManual  = "manual"
)

// Runner is the slice of the backup service the scheduler drives.
//
// It is an interface rather than a concrete dependency for one reason that matters: the scheduler's
// tests exercise claiming, heartbeating, losing a lease and reaping, and none of that should
// require an object store, a plugin process, or a container runtime to be present.
//
// Both methods are synchronous, unlike the RPC-facing RunBackup and RunVerification. The scheduler
// needs the work bound to the runner's context — a lost lease has to be able to stop it — and needs
// to know whether the backup succeeded before it can decide about verification.
type Runner interface {
	// RunBackupJob runs a backup against a job that already exists and returns when the outcome
	// has been recorded. The returned identifier is the backup row, which the verify job names.
	RunBackupJob(ctx context.Context, in BackupJob) (backupID string, err error)
	// RunVerificationJob restores a backup into a sandbox and records the verdict.
	RunVerificationJob(ctx context.Context, in VerificationJob) error
}

// BackupJob is one scheduled backup, assembled from the job row alone.
type BackupJob struct {
	JobID      string
	ScheduleID string
	InstanceID string
	MethodID   string
	Options    map[string]string
}

// VerificationJob is one scheduled verification.
type VerificationJob struct {
	JobID    string
	BackupID string
}

// Scheduler materializes due schedules into jobs, leases them, and runs them.
//
// It owns no timers that decide anything. The tick is a dumb poll; every decision is a row, which
// is what lets a restart resume mid-stream and two replicas share one estate (ADR-0013).
type Scheduler struct {
	pool    *pgxpool.Pool
	runner  Runner
	log     *slog.Logger
	cfg     config.SchedulerConfig
	owner   string
	tenetID string

	// slots bounds jobs running in this process. A verification holds a container and a spooled
	// artifact, so fifty instances verifying at 02:00 is a resource incident rather than a busy
	// night; this is the knob that prevents it.
	slots chan struct{}

	running sync.WaitGroup
	cancel  context.CancelFunc
	stopped chan struct{}

	// lastTick is read by HealthCheck. A tick loop that has stopped advancing is the failure this
	// component can suffer without anything else noticing, so it is reported rather than inferred.
	lastTick atomic.Int64
}

// New builds a scheduler. It does not start until Start is called.
func New(pool *pgxpool.Pool, runner Runner, cfg config.SchedulerConfig, log *slog.Logger) *Scheduler {
	owner := newOwnerID()
	slots := cfg.MaxConcurrentJobs
	if slots <= 0 {
		slots = 1
	}
	return &Scheduler{
		pool:    pool,
		runner:  runner,
		log:     log.With(slog.String("component", "scheduler"), slog.String("lease_owner", owner)),
		cfg:     cfg,
		owner:   owner,
		tenetID: metadb.DefaultTenantID,
		slots:   make(chan struct{}, slots),
		stopped: make(chan struct{}),
	}
}

// Start begins the tick loop. It returns immediately; Close stops it.
func (s *Scheduler) Start(ctx context.Context) {
	if !s.cfg.Enabled {
		s.log.Info("scheduler disabled; nothing will run automatically")
		close(s.stopped)
		return
	}

	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.lastTick.Store(time.Now().UnixNano())

	s.log.Info("scheduler started",
		slog.Duration("poll_interval", s.cfg.PollInterval),
		slog.Duration("lease_ttl", s.cfg.LeaseTTL),
		slog.Duration("lease_heartbeat", s.cfg.LeaseHeartbeat),
		slog.Int("max_concurrent_jobs", cap(s.slots)))

	go s.loop(loopCtx)
}

// loop polls until the context is cancelled.
//
// It never returns on an error. A poll loop that exits on the first database blip stops the entire
// product silently: no line says "the scheduler stopped", backups simply cease, and nobody finds
// out until a restore is needed. Every failure inside one tick is logged and the loop continues;
// only cancellation ends it.
func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.stopped)

	interval := s.cfg.PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping; waiting for in-flight jobs")
			s.running.Wait()
			s.log.Info("scheduler stopped")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick is one pass: materialize what is due, reap what was abandoned, claim what can run.
func (s *Scheduler) tick(ctx context.Context) {
	defer s.lastTick.Store(time.Now().UnixNano())

	if n, err := s.materialize(ctx); err != nil {
		s.log.ErrorContext(ctx, "could not turn due schedules into jobs", slog.String("error", err.Error()))
	} else if n > 0 {
		s.log.InfoContext(ctx, "scheduled jobs created", slog.Int("count", n))
	}

	if n, err := reap(ctx, s.pool); err != nil {
		s.log.ErrorContext(ctx, "could not reap expired leases", slog.String("error", err.Error()))
	} else if n > 0 {
		// Warn, not info: every one of these is a run that did not finish, and an operator who
		// never sees this line will not know why a backup is missing.
		s.log.WarnContext(ctx, "closed jobs whose runner stopped reporting",
			slog.Int64("count", n))
	}

	s.dispatch(ctx)
}

// dispatch claims and starts jobs until the concurrency budget is full or nothing is due.
func (s *Scheduler) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case s.slots <- struct{}{}:
		default:
			// Every slot is busy. Not an error, and not worth a log line every ten seconds.
			return
		}

		job, err := claim(ctx, s.pool, s.owner, s.cfg.LeaseTTL)
		if err != nil {
			<-s.slots
			if !errors.Is(err, context.Canceled) {
				s.log.ErrorContext(ctx, "could not claim a job", slog.String("error", err.Error()))
			}
			return
		}
		if job == nil {
			<-s.slots
			return
		}

		s.running.Add(1)
		go func() {
			defer s.running.Done()
			defer func() { <-s.slots }()
			s.run(ctx, job)
		}()
	}
}

// HealthCheck reports whether the tick loop is still advancing.
//
// Registered as non-critical: a stalled scheduler should degrade readiness rather than take the
// estate view offline. It is also the only way this failure becomes visible at all — a loop that
// has quietly stopped looks exactly like an estate with nothing scheduled.
func (s *Scheduler) HealthCheck(context.Context) error {
	if !s.cfg.Enabled {
		return nil
	}
	last := time.Unix(0, s.lastTick.Load())
	// Three intervals of slack: one missed tick is a slow query, three in a row is a loop that is
	// not running.
	limit := 3 * s.cfg.PollInterval
	if limit < 30*time.Second {
		limit = 30 * time.Second
	}
	if age := time.Since(last); age > limit {
		return fmt.Errorf("the scheduler has not completed a tick in %s; nothing is running automatically",
			age.Round(time.Second))
	}
	return nil
}

// Close stops claiming, cancels every job in flight, and waits for each to unwind.
//
// It cancels rather than drains, deliberately. A backup legitimately takes hours, and a control
// plane that refused to stop until one finished could never be restarted. What it does guarantee is
// the same thing the backup service guarantees: the process does not go away until every runner has
// returned, so a cancelled run still writes down that it was cancelled. A run cut off mid-write is
// precisely the orphan the reaper exists to clean up, and a clean shutdown should not create one.
//
// Ordering matters and is asserted by the control plane's main: the scheduler must stop before the
// backup service drains. The backup service's Close waits for every run it owns, and the runs the
// scheduler starts are among them — so closing the backup service first would wait on work the
// scheduler is still handing it, and shutdown would never complete.
func (s *Scheduler) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	<-s.stopped
	return nil
}
