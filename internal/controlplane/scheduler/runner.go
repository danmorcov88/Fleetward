package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

// materialize turns every due schedule into a job.
//
// The order of the two writes is deliberate and is the whole at-most-once story on this side:
// next_run_at is advanced first, by a compare-and-swap on the value we read, and only the process
// that wins that swap inserts the job. Two replicas therefore produce one job, without a
// transaction and without a lock. If this process dies between the two, the tick is lost — a
// missed backup, visible as missed. The other order would risk a duplicate, and a duplicate
// concurrent dump against a production instance is an incident (ADR-0013).
func (s *Scheduler) materialize(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, instance_id, kind, cron_expression, timezone, method_id, options,
		       verify_policy, verify_sample_percent, retention_days, next_run_at
		FROM   schedules
		WHERE  tenant_id = $1 AND is_enabled AND next_run_at IS NOT NULL AND next_run_at <= now()
		ORDER  BY next_run_at`, authn.Tenant(ctx))
	if err != nil {
		return 0, fmt.Errorf("scheduler: find due schedules: %w", err)
	}

	type due struct {
		id, instanceID, kind, expr, timezone, methodID, policy string
		options                                                map[string]string
		samplePct                                              int32
		retentionDays                                          int32
		nextRunAt                                              time.Time
	}
	var pending []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.instanceID, &d.kind, &d.expr, &d.timezone, &d.methodID,
			&d.options, &d.policy, &d.samplePct, &d.retentionDays, &d.nextRunAt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scheduler: read a due schedule: %w", err)
		}
		pending = append(pending, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scheduler: find due schedules: %w", err)
	}

	created := 0
	for _, d := range pending {
		next, err := nextRun(d.expr, d.timezone, time.Now())
		if err != nil {
			// A schedule whose expression stopped parsing — because a timezone was removed from
			// the system database, say — must not stop the loop. It is logged every tick, which is
			// noisy on purpose: this is a schedule that will never fire again.
			s.log.ErrorContext(ctx, "a schedule cannot be advanced and will not run",
				slog.String("schedule_id", d.id),
				slog.String("cron_expression", d.expr),
				slog.String("timezone", d.timezone),
				slog.String("error", err.Error()))
			continue
		}

		// The compare-and-swap. `next_run_at = $3` is the value this process read; if another
		// replica advanced it first, this updates nothing and that replica owns the tick.
		tag, err := s.pool.Exec(ctx, `
			UPDATE schedules
			SET    next_run_at = $2, last_run_at = now(), updated_at = now()
			WHERE  id = $1 AND next_run_at = $3`,
			d.id, next, d.nextRunAt)
		if err != nil {
			return created, fmt.Errorf("scheduler: advance schedule %s: %w", d.id, err)
		}
		if tag.RowsAffected() == 0 {
			continue // another replica took this tick
		}

		payload, err := jobPayload{
			MethodID:            d.methodID,
			Options:             d.options,
			VerifyPolicy:        d.policy,
			VerifySamplePercent: d.samplePct,
			RetentionDays:       d.retentionDays,
		}.encode()
		if err != nil {
			return created, err
		}

		var jobID string
		err = s.pool.QueryRow(ctx, `
			INSERT INTO jobs (tenant_id, schedule_id, instance_id, kind, state, payload, scheduled_for)
			VALUES ($1, $2, $3, $4, 'pending', $5, now())
			RETURNING id`,
			authn.Tenant(ctx), d.id, d.instanceID, d.kind, payload).Scan(&jobID)

		// idx_jobs_one_active_per_instance_kind is a partial unique index, not a check this code
		// performs: a second pending or running backup for one instance raises 23505 rather than
		// inserting. That is the intended behaviour and it means something worth seeing — the
		// previous run of this schedule has not finished yet, so the backup is taking longer than
		// the interval it was scheduled at.
		if metadb.IsUniqueViolation(err) {
			s.log.WarnContext(ctx, "skipped a scheduled run because the previous one is still active",
				slog.String("schedule_id", d.id),
				slog.String("instance_id", d.instanceID),
				slog.String("kind", d.kind))
			continue
		}
		if err != nil {
			return created, fmt.Errorf("scheduler: create a job for schedule %s: %w", d.id, err)
		}

		created++
		s.log.InfoContext(ctx, "scheduled run queued",
			slog.String("job_id", jobID),
			slog.String("schedule_id", d.id),
			slog.String("instance_id", d.instanceID),
			slog.String("kind", d.kind),
			slog.Time("next_run_at", next))
	}
	return created, nil
}

// run executes one claimed job under a lease this process holds for as long as the work lasts.
//
// The shape is the same for every kind: derive a cancellable context, heartbeat in the background,
// do the work, and give the lease back. What makes it correct is that the heartbeat can cancel the
// work — see heartbeat.
func (s *Scheduler) run(parent context.Context, job *claimedJob) {
	// job_id rides on the context rather than on the logger: telemetry's handler promotes it onto
	// every record written with a *Context method, including the ones the backup service writes
	// further down the call. Attaching it here as well would print it twice on every line.
	parent = telemetry.WithJobID(parent, job.ID)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	log := s.log.With(
		slog.String("instance_id", job.InstanceID),
		slog.String("kind", job.Kind))

	lost := make(chan struct{})
	go s.heartbeat(ctx, cancel, job.ID, lost)

	payload, err := decodePayload(job.Payload)
	if err != nil {
		log.ErrorContext(ctx, "the job payload could not be read", slog.String("error", err.Error()))
		s.close(ctx, job.ID, "failed", err.Error(), log)
		return
	}

	started := time.Now()
	var runErr error
	switch job.Kind {
	case kindBackup:
		runErr = s.runBackup(ctx, job, payload, log)
	case kindVerify:
		runErr = s.runVerify(ctx, job, payload, log)
	case kindObserve:
		runErr = s.runner.RunObservationJob(ctx, ObservationJob{
			JobID:      job.ID,
			InstanceID: job.InstanceID,
		})
	case kindDiscovery:
		runErr = s.runner.RunDiscoveryJob(ctx, DiscoveryJob{
			JobID:      job.ID,
			InstanceID: job.InstanceID,
		})
	default:
		runErr = fmt.Errorf("%w: the scheduler does not run %q jobs", ErrUnsupported, job.Kind)
	}

	// Losing the lease is not a job outcome, so nothing about the job is written here. Another
	// process has already decided what this job says, and a ghost runner overwriting that verdict
	// is exactly what the lease exists to prevent.
	select {
	case <-lost:
		log.WarnContext(parent, "abandoned a job whose lease was lost",
			slog.Duration("ran_for", time.Since(started)))
		return
	default:
	}

	if runErr != nil {
		log.ErrorContext(ctx, "scheduled job failed",
			slog.String("error", runErr.Error()),
			slog.Duration("duration", time.Since(started)))
		s.close(parent, job.ID, "failed", runErr.Error(), log)
		return
	}

	log.InfoContext(ctx, "scheduled job finished", slog.Duration("duration", time.Since(started)))

	// An observation and a health probe both write no row that is about the job — one updates the
	// backups it found, the other the instance it probed — so there is no transaction for the
	// outcome to be folded into, and nothing else will close it. A job left running is not
	// cosmetic: idx_jobs_one_active_per_instance_kind then blocks every later job of that kind for
	// that instance, and the polling silently stops.
	if job.Kind == kindObserve || job.Kind == kindDiscovery {
		s.close(parent, job.ID, "succeeded", "", log)
	}

	// The backup and verification services write the job's terminal state themselves, in the same
	// transaction as the row that explains it. All that is left is to stop holding the lease.
	//
	// Detached from the runner's context, like every other write on the way out: a job that
	// finished during shutdown should still let go of its lease rather than leave a terminal row
	// wearing a dead process's name.
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancelRelease()
	if err := release(releaseCtx, s.pool, job.ID, s.owner); err != nil {
		log.ErrorContext(releaseCtx, "could not release the lease", slog.String("error", err.Error()))
	}
}

// heartbeat renews the lease until the work ends, and cancels the work if the lease is gone.
//
// The subtle case in this whole slice. The renewal ends `WHERE id = $1 AND lease_owner = $2`, and
// when that updates **zero rows** it is not a database error and must not be logged as one: it
// means the lease is gone, another process has already written this job's outcome, and this runner
// is a ghost holding a connection to a production server and possibly a sandbox container.
//
// So it cancels the work's own context. The context is the only thing that reaches down through
// the backup service, the plugin client, and the native tool it is driving.
func (s *Scheduler) heartbeat(ctx context.Context, cancel context.CancelFunc, jobID string, lost chan<- struct{}) {
	interval := s.cfg.LeaseHeartbeat
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Detached from the run's context so that a heartbeat is not cancelled by the very thing it
		// is trying to detect, and bounded so that an unresponsive database cannot make the
		// heartbeat itself hang past the lease.
		renewCtx, done := context.WithTimeout(context.WithoutCancel(ctx), interval)
		err := renew(renewCtx, s.pool, jobID, s.owner, s.cfg.LeaseTTL)
		done()

		switch {
		case err == nil:
		case errors.Is(err, errLeaseLost):
			s.log.WarnContext(ctx, "lost the lease on a running job; stopping the work",
				slog.String("job_id", jobID),
				slog.String("detail", "another process has already recorded this job's outcome"))
			close(lost)
			cancel()
			return
		default:
			// A transient failure to renew is not a lost lease. The lease outlives several
			// heartbeats by design, so the right response is to log and try again.
			s.log.WarnContext(ctx, "could not renew the lease; will retry",
				slog.String("job_id", jobID),
				slog.String("error", err.Error()))
		}
	}
}

// runBackup runs one scheduled backup and, if the policy says so, queues its verification.
func (s *Scheduler) runBackup(ctx context.Context, job *claimedJob, p jobPayload, log *slog.Logger) error {
	backupID, err := s.runner.RunBackupJob(ctx, BackupJob{
		JobID:      job.ID,
		ScheduleID: job.ScheduleID,
		InstanceID: job.InstanceID,
		MethodID:   p.MethodID,
		Options:    p.Options,

		RetentionDays: p.RetentionDays,
	})
	if err != nil {
		return err
	}

	// Verification is queued as its own job rather than chained in-process. That is what puts the
	// policy decision in the job table where it can be read afterwards, and what makes a
	// verification compete for the same concurrency budget as everything else — each one holds a
	// container and a spooled artifact.
	if !s.shouldVerify(p) {
		log.InfoContext(ctx, "backup will not be verified automatically",
			slog.String("backup_id", backupID),
			slog.String("verify_policy", p.VerifyPolicy),
			slog.Int("verify_sample_percent", int(p.VerifySamplePercent)))
		return nil
	}
	return s.enqueueVerify(ctx, job, backupID, log)
}

// shouldVerify applies the schedule's verification policy to this one run.
func (s *Scheduler) shouldVerify(p jobPayload) bool {
	switch p.VerifyPolicy {
	case verifySampled:
		if p.VerifySamplePercent <= 0 {
			return false
		}
		if p.VerifySamplePercent >= 100 {
			return true
		}
		return rand.Int32N(100) < p.VerifySamplePercent //nolint:gosec // sampling, not security
	case verifyManual:
		return false
	default:
		// Unset or "always". Defaulting to verifying is the safe direction: an unverified backup
		// that looks verified is the state this product exists to eliminate.
		return true
	}
}

// enqueueVerify inserts the verify job that will prove this backup restorable.
func (s *Scheduler) enqueueVerify(ctx context.Context, job *claimedJob, backupID string, log *slog.Logger) error {
	payload, err := jobPayload{BackupID: backupID}.encode()
	if err != nil {
		return err
	}

	var scheduleID any
	if job.ScheduleID != "" {
		scheduleID = job.ScheduleID
	}

	// Detached and bounded: the backup succeeded, and losing its verification because the process
	// was asked to stop a moment later would leave an unproven artifact with nothing queued to
	// prove it.
	enqueueCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	var verifyJobID string
	err = s.pool.QueryRow(enqueueCtx, `
		INSERT INTO jobs (tenant_id, schedule_id, instance_id, kind, state, payload, scheduled_for)
		VALUES ($1, $2, $3, 'verify', 'pending', $4, now())
		RETURNING id`,
		job.TenantID, scheduleID, job.InstanceID, payload).Scan(&verifyJobID)
	if metadb.IsUniqueViolation(err) {
		// A verification of an earlier backup on this instance is still in flight. Skipping is
		// correct — sandbox capacity is the scarce resource, and the next scheduled backup will
		// queue its own verification.
		log.WarnContext(ctx, "skipped queuing a verification because one is already active for this instance",
			slog.String("backup_id", backupID))
		return nil
	}
	if err != nil {
		return fmt.Errorf("scheduler: queue the verification for backup %s: %w", backupID, err)
	}

	log.InfoContext(ctx, "verification queued",
		slog.String("verify_job_id", verifyJobID),
		slog.String("backup_id", backupID))
	return nil
}

// runVerify runs one scheduled verification.
func (s *Scheduler) runVerify(ctx context.Context, job *claimedJob, p jobPayload, _ *slog.Logger) error {
	if p.BackupID == "" {
		return errors.New("scheduler: this verify job names no backup")
	}
	return s.runner.RunVerificationJob(ctx, VerificationJob{JobID: job.ID, BackupID: p.BackupID})
}

// close writes a terminal state for a job that failed before the backup service could record one.
func (s *Scheduler) close(ctx context.Context, jobID, state, message string, log *slog.Logger) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := finish(closeCtx, s.pool, jobID, s.owner, state, message); err != nil {
		log.ErrorContext(closeCtx, "could not record a job's outcome", slog.String("error", err.Error()))
	}
}
