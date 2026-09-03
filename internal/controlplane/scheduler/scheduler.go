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
	kindBackup    = "backup"
	kindVerify    = "verify"
	kindObserve   = "observe"
	kindDiscovery = "discovery"

	verifyAlways  = "always"
	verifySampled = "sampled"
	verifyManual  = "manual"
)

// Runner is the slice of the rest of the control plane that the scheduler drives.
//
// It is an interface rather than a concrete dependency for one reason that matters: the scheduler's
// tests exercise claiming, heartbeating, losing a lease and reaping, and none of that should
// require an object store, a plugin process, or a container runtime to be present.
//
// Every method is synchronous, unlike the RPC-facing RunBackup and RunVerification. The scheduler
// needs the work bound to the runner's context — a lost lease has to be able to stop it — and needs
// to know whether the backup succeeded before it can decide about verification.
type Runner interface {
	// RunBackupJob runs a backup against a job that already exists and returns when the outcome
	// has been recorded. The returned identifier is the backup row, which the verify job names.
	RunBackupJob(ctx context.Context, in BackupJob) (backupID string, err error)
	// RunVerificationJob restores a backup into a sandbox and records the verdict.
	RunVerificationJob(ctx context.Context, in VerificationJob) error
	// RunObservationJob reads the engine's own record of backups Fleetward did not take, and
	// records what it finds (ADR-0015). It touches nothing on the instance it reads.
	RunObservationJob(ctx context.Context, in ObservationJob) error
	// RunDiscoveryJob probes one instance and records its health, so that the estate's health is
	// an answer somebody can put a date on rather than whatever the last human left behind.
	RunDiscoveryJob(ctx context.Context, in DiscoveryJob) error
	// SweepRetention deletes the artifacts of managed backups that have outlived the retention
	// their schedule declared.
	//
	// It is the one thing on this interface that is not a job. Retention is an estate-wide property
	// rather than a per-instance one, and it is idempotent in a way a backup is not — the state
	// transition is its own guard, so two control planes sweeping at once is not a race to lose and
	// no lease is needed (ADR-0030).
	SweepRetention(ctx context.Context) (RetentionOutcome, error)
}

// RetentionOutcome is what one sweep did. There is no job row behind a sweep, so this is what the
// log line is made of, and the log line plus the rows themselves are the whole account.
type RetentionOutcome struct {
	Expired          int
	ArtifactsDeleted int
	BytesReclaimed   int64
	// Unreachable counts artifacts that could not be deleted this time and stay queued for the next
	// sweep. Nothing is lost when this is non-zero; it is here so an object store that has been
	// refusing all week is visible rather than silent.
	Unreachable int
}

// Empty reports whether the sweep did nothing, which is the ordinary case.
func (o RetentionOutcome) Empty() bool {
	return o.Expired == 0 && o.ArtifactsDeleted == 0 && o.Unreachable == 0
}

// BackupJob is one scheduled backup, assembled from the job row alone.
type BackupJob struct {
	JobID      string
	ScheduleID string
	InstanceID string
	MethodID   string
	Options    map[string]string
	// RetentionDays becomes the stamped expiry on the artifact this run produces (ADR-0031). Zero
	// stamps none, and an artifact with no expiry is never deleted by retention.
	RetentionDays int32
}

// VerificationJob is one scheduled verification.
type VerificationJob struct {
	JobID    string
	BackupID string
}

// ObservationJob is one scheduled read of an instance's backup history.
type ObservationJob struct {
	JobID      string
	InstanceID string
}

// DiscoveryJob is one scheduled health probe of an instance.
//
// The kind is called `discovery` because that is the name the CHECK constraint has carried since
// the schema was written, and widening a constraint to rename a value nobody has typed yet would
// be a migration spent on vocabulary. What it does is probe and record health; it does not re-run
// the plugin's Discover to refresh topology. The name is older than the job.
type DiscoveryJob struct {
	JobID      string
	InstanceID string
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

	// retention paces the one estate-wide thing on the tick that is not a job. Only Enabled and
	// Interval are read here; what a sweep may actually delete is the runner's business, and the two
	// sides refuse independently.
	retention config.RetentionConfig

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

	// lastSweep and sweeping pace retention. It runs on the tick beside the reaper rather than as a
	// scheduled job (ADR-0030), but far less often than the tick: a sweep is estate-wide and it
	// destroys things, and there is no reason for an artifact to disappear within seconds of its
	// expiry rather than within the hour. Zero means "not yet", so the first tick after a start
	// sweeps — which is also how a sweep interrupted by a crash gets finished promptly.
	lastSweep atomic.Int64
	// sweeping keeps one process to one sweep at a time. Two concurrent sweeps would be correct —
	// the design does not rely on this — but they would do the same work twice.
	sweeping atomic.Bool
}

// New builds a scheduler. It does not start until Start is called.
func New(pool *pgxpool.Pool, runner Runner, cfg config.SchedulerConfig, retention config.RetentionConfig, log *slog.Logger) *Scheduler {
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

		retention: retention,
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
		slog.Int("max_concurrent_jobs", cap(s.slots)),
		slog.Bool("retention_enabled", s.retention.Enabled),
		slog.Duration("retention_interval", s.retention.Interval))

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

	s.maybeSweepRetention(ctx)

	s.dispatch(ctx)
}

// maybeSweepRetention starts a retention sweep if one is due and none is already running here.
//
// It runs in the background rather than inline, because a sweep deletes up to MaxPerSweep objects
// over the network and a tick that waited for that would stop claiming jobs meanwhile. It joins
// s.running so that Close still waits for it: a sweep cut off between expiring a row and deleting
// its object leaves a leftover, and while that leftover is designed to be picked up by the next
// sweep, a clean shutdown has no business creating one.
func (s *Scheduler) maybeSweepRetention(ctx context.Context) {
	if !s.retention.Enabled {
		return
	}
	interval := s.retention.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	if last := s.lastSweep.Load(); last != 0 && time.Since(time.Unix(0, last)) < interval {
		return
	}
	if !s.sweeping.CompareAndSwap(false, true) {
		return
	}
	s.lastSweep.Store(time.Now().UnixNano())

	s.running.Add(1)
	go func() {
		defer s.running.Done()
		defer s.sweeping.Store(false)

		started := time.Now()
		outcome, err := s.runner.SweepRetention(ctx)
		if err != nil {
			s.log.ErrorContext(ctx, "the retention sweep did not finish",
				slog.String("error", err.Error()),
				slog.Duration("ran_for", time.Since(started)))
			return
		}
		if outcome.Empty() {
			return // nothing happened, and an hourly line saying so is noise
		}
		// Info rather than warn: unlike a reaped job, everything counted here is what the operator
		// asked for. It is logged at all because deleting an artifact is the one thing this product
		// does that cannot be undone, and there is no job row to read afterwards.
		s.log.InfoContext(ctx, "retention sweep finished",
			slog.Int("expired", outcome.Expired),
			slog.Int("artifacts_deleted", outcome.ArtifactsDeleted),
			slog.Int64("bytes_reclaimed", outcome.BytesReclaimed),
			slog.Int("unreachable", outcome.Unreachable),
			slog.Duration("duration", time.Since(started)))
	}()
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
