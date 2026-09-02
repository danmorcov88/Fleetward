// Package scheduler turns recurring intent into work that runs without anyone asking.
//
// It is the component that closes the gap between "Fleetward can take a verified backup" and
// "Fleetward takes verified backups". Three pieces do that, and they are deliberately small:
//
//   - a service (this file) through which a human declares a schedule;
//   - a tick loop (scheduler.go) that materializes due schedules into `jobs` rows, reaps the
//     leases of runners that disappeared, and claims what is due;
//   - a runner (runner.go) that holds a lease while the work happens and gives it up when the
//     work — or the lease — ends.
//
// The clock is `schedules.next_run_at` in PostgreSQL, never an in-process timer, and a job is
// claimed by one `UPDATE ... RETURNING` rather than by a transaction (ADR-0013).
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
)

// Sentinel errors. The gRPC layer maps them to status codes and is the only thing that decides what
// a client sees.
var (
	// ErrNotFound reports that no such schedule exists in this tenant.
	ErrNotFound = errors.New("not found")
	// ErrInvalidArgument reports a malformed request: a cron expression that does not parse, an
	// unknown timezone, a sample percentage outside 0-100.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrUnsupported reports a schedule kind the scheduler does not run yet.
	ErrUnsupported = errors.New("unsupported")
)

// Service is the schedule and job store: everything a human declares, and everything the scheduler
// did about it.
type Service struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	tenantID string
}

// NewService builds the service. The tenant is fixed until the authorization spine lands in B6,
// exactly as in inventory and backup.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{
		pool:     pool,
		log:      log.With(slog.String("component", "scheduler")),
		tenantID: metadb.DefaultTenantID,
	}
}

// CreateScheduleInput describes a new recurring intent.
type CreateScheduleInput struct {
	InstanceID      string
	Kind            string
	CronExpression  string
	Timezone        string
	MethodID        string
	Options         map[string]string
	VerifyPolicy    string
	VerifySamplePct int32
	RetentionDays   int32
	// ExpectedCron and ExpectedGraceMinutes declare what a backup of this instance is supposed to
	// look like, which is the half of "declare what should be true, detect what actually is" that
	// no amount of observation can supply. Optional: a schedule without them still collects
	// history, and adherence reports that nothing was declared rather than guessing.
	ExpectedCron         string
	ExpectedGraceMinutes int32
}

// CreateSchedule validates the intent and computes its first run.
//
// next_run_at is set here rather than left NULL because it is the clock: a schedule with no next
// run is a schedule the tick loop will never see. Computing it at creation also turns a bad cron
// expression or an unknown timezone into a rejected request, instead of into a schedule that sits
// in the table doing nothing and reports no reason.
func (s *Service) CreateSchedule(ctx context.Context, in CreateScheduleInput) (*fwv1.Schedule, error) {
	instanceID, err := requireUUID("instance_id", in.InstanceID)
	if err != nil {
		return nil, err
	}

	kind := in.Kind
	if kind == "" {
		kind = kindBackup
	}
	// `schedules.kind` also permits 'discovery' and 'metrics', and refusing those with a message
	// that says which slice owns them is more useful than accepting a schedule that would silently
	// never fire.
	if kind != kindBackup && kind != kindObserve {
		return nil, fmt.Errorf("%w: only %q and %q schedules run today; %q schedules arrive with the estate view",
			ErrUnsupported, kindBackup, kindObserve, kind)
	}

	timezone := in.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	// Both are validated before anything is written, and nextRun exercises both.
	next, err := nextRun(in.CronExpression, timezone, time.Now())
	if err != nil {
		return nil, err
	}

	policy := in.VerifyPolicy
	if policy == "" {
		policy = verifyAlways
	}
	if policy != verifyAlways && policy != verifySampled && policy != verifyManual {
		return nil, fmt.Errorf("%w: verify policy must be %q, %q, or %q",
			ErrInvalidArgument, verifyAlways, verifySampled, verifyManual)
	}

	pct := in.VerifySamplePct
	if policy == verifySampled && (pct < 0 || pct > 100) {
		return nil, fmt.Errorf("%w: verify_sample_percent must be between 0 and 100", ErrInvalidArgument)
	}
	if policy != verifySampled {
		// The column carries a CHECK, and a percentage that does not apply is still stored as
		// something. 100 keeps "always" readable in the table without a second column to say so.
		pct = 100
	}

	retention := in.RetentionDays
	if retention == 0 {
		retention = 30
	}
	if retention <= 0 {
		return nil, fmt.Errorf("%w: retention_days must be positive", ErrInvalidArgument)
	}

	options := in.Options
	if options == nil {
		options = map[string]string{}
	}

	expectedCron, grace, err := validateExpectation(in.ExpectedCron, in.ExpectedGraceMinutes, timezone)
	if err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		INSERT INTO schedules (tenant_id, instance_id, kind, cron_expression, timezone,
		                       method_id, options, verify_policy, verify_sample_percent,
		                       retention_days, next_run_at, expected_cron, expected_grace_minutes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+scheduleColumns,
		s.tenantID, instanceID, kind, in.CronExpression, timezone,
		in.MethodID, options, policy, pct, retention, next, expectedCron, grace)
	// pgx may surface a constraint violation either here or on the first read, depending on when
	// the round trip completes, so both are checked. Scheduling against an instance that does not
	// exist is the caller's mistake and reads as 404, not as an internal error.
	if err == nil {
		defer rows.Close()
		var schedules []*fwv1.Schedule
		schedules, err = scanSchedules(rows)
		if err == nil {
			if len(schedules) == 0 {
				return nil, fmt.Errorf("scheduler: create schedule: the insert returned no row")
			}
			s.log.InfoContext(ctx, "schedule created",
				slog.String("schedule_id", schedules[0].GetId()),
				slog.String("instance_id", instanceID),
				slog.String("cron_expression", in.CronExpression),
				slog.String("timezone", timezone),
				slog.Time("next_run_at", next))
			return schedules[0], nil
		}
	}
	if metadb.IsForeignKeyViolation(err) {
		return nil, fmt.Errorf("%w: instance %s", ErrNotFound, instanceID)
	}
	return nil, fmt.Errorf("scheduler: create schedule: %w", err)
}

// ListSchedules returns the tenant's schedules, optionally for one instance.
func (s *Service) ListSchedules(ctx context.Context, instanceID string) ([]*fwv1.Schedule, error) {
	if instanceID != "" {
		id, err := requireUUID("instance_id", instanceID)
		if err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, `SELECT `+scheduleColumns+`
			FROM schedules WHERE tenant_id = $1 AND instance_id = $2 ORDER BY created_at`,
			s.tenantID, id)
		if err != nil {
			return nil, fmt.Errorf("scheduler: list schedules: %w", err)
		}
		defer rows.Close()
		return scanSchedules(rows)
	}

	rows, err := s.pool.Query(ctx, `SELECT `+scheduleColumns+`
		FROM schedules WHERE tenant_id = $1 ORDER BY created_at`, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list schedules: %w", err)
	}
	defer rows.Close()
	return scanSchedules(rows)
}

// GetSchedule returns one schedule.
func (s *Service) GetSchedule(ctx context.Context, scheduleID string) (*fwv1.Schedule, error) {
	id, err := requireUUID("schedule_id", scheduleID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+scheduleColumns+`
		FROM schedules WHERE id = $1 AND tenant_id = $2`, id, s.tenantID)
	if err != nil {
		return nil, fmt.Errorf("scheduler: get schedule: %w", err)
	}
	defer rows.Close()

	schedules, err := scanSchedules(rows)
	if err != nil {
		return nil, err
	}
	if len(schedules) == 0 {
		return nil, fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	return schedules[0], nil
}

// SetScheduleEnabled pauses or resumes a schedule.
//
// Resuming recomputes next_run_at from now. Without that, a schedule paused for a week would come
// back with a next run a week in the past, and the tick loop would fire it immediately — which is
// how a maintenance window ends with an unplanned backup the moment someone unpauses.
func (s *Service) SetScheduleEnabled(ctx context.Context, scheduleID string, enabled bool) (*fwv1.Schedule, error) {
	current, err := s.GetSchedule(ctx, scheduleID)
	if err != nil {
		return nil, err
	}

	var next any
	if enabled {
		n, err := nextRun(current.GetCronExpression(), current.GetTimezone(), time.Now())
		if err != nil {
			return nil, err
		}
		next = n
	} else {
		// A paused schedule has no next run, which is both true and what keeps it out of the
		// tick loop's query rather than relying on the is_enabled predicate alone.
		next = nil
	}

	rows, err := s.pool.Query(ctx, `
		UPDATE schedules SET is_enabled = $3, next_run_at = $4, updated_at = now()
		WHERE id = $1 AND tenant_id = $2
		RETURNING `+scheduleColumns,
		current.GetId(), s.tenantID, enabled, next)
	if err != nil {
		return nil, fmt.Errorf("scheduler: set schedule enabled: %w", err)
	}
	defer rows.Close()

	schedules, err := scanSchedules(rows)
	if err != nil {
		return nil, err
	}
	if len(schedules) == 0 {
		return nil, fmt.Errorf("%w: schedule %s", ErrNotFound, current.GetId())
	}

	s.log.InfoContext(ctx, "schedule enablement changed",
		slog.String("schedule_id", current.GetId()),
		slog.Bool("is_enabled", enabled))
	return schedules[0], nil
}

// DeleteSchedule removes a schedule. Jobs it already created keep their history: `jobs.schedule_id`
// is ON DELETE SET NULL, so what ran stays visible after the intent behind it is gone.
func (s *Service) DeleteSchedule(ctx context.Context, scheduleID string) error {
	id, err := requireUUID("schedule_id", scheduleID)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM schedules WHERE id = $1 AND tenant_id = $2`, id, s.tenantID)
	if err != nil {
		return fmt.Errorf("scheduler: delete schedule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: schedule %s", ErrNotFound, id)
	}
	s.log.InfoContext(ctx, "schedule deleted", slog.String("schedule_id", id))
	return nil
}

// ListJobsFilter narrows the job listing.
type ListJobsFilter struct {
	InstanceID string
	ScheduleID string
	State      string
	PageSize   int32
}

// ListJobs reports what the scheduler actually did.
//
// This is the only surface on which a skipped run, a job another replica leased, or a job the
// reaper closed after a crash is visible at all. Without it the scheduler is a black box, and a
// black box that touches production databases is not something to install.
func (s *Service) ListJobs(ctx context.Context, f ListJobsFilter) ([]*fwv1.Job, error) {
	const defaultPageSize, maxPageSize = 50, 500
	limit := f.PageSize
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}

	var instanceID, scheduleID any
	if f.InstanceID != "" {
		id, err := requireUUID("instance_id", f.InstanceID)
		if err != nil {
			return nil, err
		}
		instanceID = id
	}
	if f.ScheduleID != "" {
		id, err := requireUUID("schedule_id", f.ScheduleID)
		if err != nil {
			return nil, err
		}
		scheduleID = id
	}
	var state any
	if f.State != "" {
		state = f.State
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, tenant_id, instance_id, COALESCE(schedule_id::text, ''), kind, state,
		       scheduled_for, COALESCE(lease_owner, ''), lease_expires_at, heartbeat_at,
		       attempts, started_at, finished_at, error_message, created_at
		FROM   jobs
		WHERE  tenant_id = $1
		  AND  ($2::uuid IS NULL OR instance_id = $2)
		  AND  ($3::uuid IS NULL OR schedule_id = $3)
		  AND  ($4::text IS NULL OR state = $4)
		ORDER  BY created_at DESC
		LIMIT  $5`,
		s.tenantID, instanceID, scheduleID, state, limit)
	if err != nil {
		return nil, fmt.Errorf("scheduler: list jobs: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.Job
	for rows.Next() {
		var (
			j                                    fwv1.Job
			kind, state                          string
			leaseExpires, heartbeat              *time.Time
			scheduledFor, createdAt              time.Time
			startedAt, finishedAt                *time.Time
			id, tenantID, instanceID, scheduleID string
			leaseOwner, errorMessage             string
			attempts                             int32
		)
		if err := rows.Scan(&id, &tenantID, &instanceID, &scheduleID, &kind, &state,
			&scheduledFor, &leaseOwner, &leaseExpires, &heartbeat,
			&attempts, &startedAt, &finishedAt, &errorMessage, &createdAt); err != nil {
			return nil, fmt.Errorf("scheduler: read a job: %w", err)
		}
		j.Id = id
		j.TenantId = tenantID
		j.InstanceId = instanceID
		j.ScheduleId = scheduleID
		j.Kind = jobKindFromName(kind)
		j.State = jobStateFromName(state)
		j.ScheduledFor = timestamppb.New(scheduledFor)
		j.LeaseOwner = leaseOwner
		j.LeaseExpiresAt = timestampOrNil(leaseExpires)
		j.HeartbeatAt = timestampOrNil(heartbeat)
		j.Attempts = attempts
		j.StartedAt = timestampOrNil(startedAt)
		j.FinishedAt = timestampOrNil(finishedAt)
		j.ErrorMessage = errorMessage
		j.CreatedAt = timestamppb.New(createdAt)
		out = append(out, &j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduler: list jobs: %w", err)
	}
	return out, nil
}

// -----------------------------------------------------------------------------------------------
// Row mapping
// -----------------------------------------------------------------------------------------------

const scheduleColumns = `id, tenant_id, instance_id, kind, cron_expression, timezone,
	method_id, options, verify_policy, verify_sample_percent, retention_days, is_enabled,
	next_run_at, last_run_at, created_at, expected_cron, expected_grace_minutes`

// defaultExpectedGraceMinutes is how late a backup may be, when a declaration names a schedule but
// no tolerance.
//
// Zero would be the literal reading and it is useless: it demands a backup complete in the same
// instant it was due, so every instance on the estate would report as missing one. Two hours is what
// a nightly window actually needs — it absorbs a run that started on time and took longer than
// usual, and it absorbs the one hour an engine that records local time without an offset can be
// wrong by across a daylight-saving change.
const defaultExpectedGraceMinutes = 120

// validateExpectation checks the declaration before it is stored, so a cron expression that does
// not parse is a rejected request rather than an instance that silently reports NOT_DECLARED.
func validateExpectation(expr string, grace int32, timezone string) (string, int32, error) {
	if expr == "" {
		if grace != 0 {
			return "", 0, fmt.Errorf(
				"%w: expected_grace_minutes means nothing without expected_cron: it says how late a "+
					"backup may be, and nothing has said when one is due", ErrInvalidArgument)
		}
		return "", 0, nil
	}
	if grace < 0 {
		return "", 0, fmt.Errorf("%w: expected_grace_minutes cannot be negative", ErrInvalidArgument)
	}
	if grace == 0 {
		grace = defaultExpectedGraceMinutes
	}
	// The same parse and the same location lookup the schedule's own cron gets, and for the same
	// reason: 02:00 for a Bucharest server is a different UTC instant in summer than in winter.
	if _, err := nextRun(expr, timezone, time.Now()); err != nil {
		return "", 0, fmt.Errorf("%w (expected_cron)", err)
	}
	return expr, grace, nil
}

func scanSchedules(rows pgx.Rows) ([]*fwv1.Schedule, error) {
	var out []*fwv1.Schedule
	for rows.Next() {
		var (
			s                    fwv1.Schedule
			kind, policy         string
			options              map[string]string
			nextRunAt, lastRunAt *time.Time
			createdAt            time.Time
		)
		if err := rows.Scan(&s.Id, &s.TenantId, &s.InstanceId, &kind, &s.CronExpression, &s.Timezone,
			&s.MethodId, &options, &policy, &s.VerifySamplePercent, &s.RetentionDays, &s.IsEnabled,
			&nextRunAt, &lastRunAt, &createdAt, &s.ExpectedCron, &s.ExpectedGraceMinutes); err != nil {
			return nil, fmt.Errorf("scheduler: read a schedule: %w", err)
		}
		s.Kind = jobKindFromName(kind)
		s.VerifyPolicy = verifyPolicyFromName(policy)
		s.Options = options
		s.NextRunAt = timestampOrNil(nextRunAt)
		s.LastRunAt = timestampOrNil(lastRunAt)
		s.CreatedAt = timestamppb.New(createdAt)
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scheduler: read schedules: %w", err)
	}
	return out, nil
}

func timestampOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	return timestamppb.New(*t)
}

func requireUUID(field, value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidArgument, field)
	}
	if !metadb.IsUUID(value) {
		return "", fmt.Errorf("%w: %s is not a valid identifier", ErrInvalidArgument, field)
	}
	return value, nil
}

// jobPayload is what a job carries from the schedule that created it.
//
// It is a snapshot, taken when the job row was inserted, and the runner never re-reads the
// schedule. A schedule edited at 02:15 must not change what the 02:00 run was asked to do — the job
// row is the record of what was actually asked, and that is what makes the job table answerable
// after the fact rather than merely descriptive.
type jobPayload struct {
	MethodID string            `json:"method_id,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
	// VerifyPolicy and VerifySamplePercent decide whether a successful backup enqueues a verify
	// job. Carrying them here is what makes "why was this backup never verified" answerable by
	// looking at the row rather than by reading this package.
	VerifyPolicy        string `json:"verify_policy,omitempty"`
	VerifySamplePercent int32  `json:"verify_sample_percent,omitempty"`
	// BackupID is set on a verify job and names what it must prove.
	BackupID string `json:"backup_id,omitempty"`
}

func (p jobPayload) encode() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("scheduler: encode the job payload: %w", err)
	}
	return b, nil
}

func decodePayload(raw []byte) (jobPayload, error) {
	var p jobPayload
	if len(raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("scheduler: decode the job payload: %w", err)
	}
	return p, nil
}
