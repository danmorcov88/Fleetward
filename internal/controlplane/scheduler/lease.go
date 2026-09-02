package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newOwnerID builds this process's lease identity: <hostname>/<pid>/<process-uuid>.
//
// The random suffix is the part that earns its place. A control plane restarted on the same host
// can be handed a recycled pid, and "host-1/4711" would then be indistinguishable from the process
// that died holding a lease — which would let the new process renew a lease it never claimed, and
// with it run a job another runner still believes is its own. The suffix is generated once, at
// startup, so no two processes ever share an owner string.
func newOwnerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to refuse to schedule. Falling back to the clock
		// keeps the identity unique in practice, which is all this suffix has to be.
		return fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// claimedJob is a job this process now owns. Every field was read in the same statement that took
// the lease, so nothing here can have been changed by another runner in between.
type claimedJob struct {
	ID         string
	TenantID   string
	InstanceID string
	ScheduleID string
	Kind       string
	Payload    []byte
	Attempts   int32
}

// claimSQL takes exactly one due job in a single statement.
//
// One statement, not SELECT ... FOR UPDATE SKIP LOCKED followed by an UPDATE. A single statement is
// atomic without a transaction, and it cannot leave a job claimed in the database but unrecorded in
// the process — which is precisely the window the two-statement version opens, and the window in
// which a job is lost.
//
// The subselect is not a second statement: PostgreSQL's UPDATE has no LIMIT, so selecting the one
// row to take has to happen inside the WHERE clause. SKIP LOCKED is there so that two runners
// racing pick different rows rather than queueing behind one.
//
// Only 'pending' jobs are claimable. A 'running' job whose lease has expired is not re-claimed —
// it is failed by the reaper (ADR-0025) — which is what makes the state transition pending ->
// running happen exactly once per job, ever.
const claimSQL = `
	UPDATE jobs
	SET    state            = 'running',
	       lease_owner      = $1,
	       lease_expires_at = now() + $2::interval,
	       heartbeat_at     = now(),
	       attempts         = attempts + 1,
	       started_at       = COALESCE(started_at, now()),
	       updated_at       = now()
	WHERE  id = (
	    SELECT id FROM jobs
	    WHERE  state = 'pending'
	      AND  scheduled_for <= now()
	    ORDER  BY scheduled_for
	    LIMIT  1
	    FOR UPDATE SKIP LOCKED
	)
	RETURNING id, tenant_id, instance_id, COALESCE(schedule_id::text, ''), kind, payload, attempts`

// claim takes the next due job, or reports that there was none.
func claim(ctx context.Context, pool *pgxpool.Pool, owner string, ttl time.Duration) (*claimedJob, error) {
	var j claimedJob
	err := pool.QueryRow(ctx, claimSQL, owner, interval(ttl)).
		Scan(&j.ID, &j.TenantID, &j.InstanceID, &j.ScheduleID, &j.Kind, &j.Payload, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil //nolint:nilnil // "no work" is the common case, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("scheduler: claim a job: %w", err)
	}
	return &j, nil
}

// errLeaseLost reports that this runner no longer owns the job it is running.
//
// It is returned when a heartbeat updates zero rows. Zero rows is not a database error and must
// never be logged as one: it means the lease is gone, someone else has already written this job's
// outcome, and this runner is a ghost holding a connection to a production server.
var errLeaseLost = errors.New("scheduler: the lease on this job is no longer held by this process")

// renew extends the lease. It returns errLeaseLost if the job is no longer ours.
//
// The `lease_owner = $2` and `state = 'running'` predicates together are the whole check: another
// process claiming the job changes the owner, and the reaper closing it changes the state. Either
// makes this statement match nothing.
func renew(ctx context.Context, pool *pgxpool.Pool, jobID, owner string, ttl time.Duration) error {
	tag, err := pool.Exec(ctx, `
		UPDATE jobs
		SET    lease_expires_at = now() + $3::interval,
		       heartbeat_at     = now(),
		       updated_at       = now()
		WHERE  id = $1 AND lease_owner = $2 AND state = 'running'`,
		jobID, owner, interval(ttl))
	if err != nil {
		return fmt.Errorf("scheduler: renew the lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errLeaseLost
	}
	return nil
}

// release drops the lease on a job whose outcome another component has already recorded.
//
// The backup service closes the job row itself, so this only clears the lease columns and only
// while they still name us. A job the reaper took is left exactly as the reaper wrote it.
func release(ctx context.Context, pool *pgxpool.Pool, jobID, owner string) error {
	_, err := pool.Exec(ctx, `
		UPDATE jobs
		SET    lease_owner      = NULL,
		       lease_expires_at = NULL,
		       updated_at       = now()
		WHERE  id = $1 AND lease_owner = $2`,
		jobID, owner)
	if err != nil {
		return fmt.Errorf("scheduler: release the lease: %w", err)
	}
	return nil
}

// finish closes a job this runner still owns, for the paths where nothing else writes the outcome.
//
// The backup and verification services write their own terminal state, so this is the fallback for
// a job that failed before reaching them — a resolvable instance that is not resolvable any more, a
// plugin that is gone. The lease predicate is what keeps it from overwriting the reaper's verdict.
func finish(ctx context.Context, pool *pgxpool.Pool, jobID, owner, state, message string) error {
	_, err := pool.Exec(ctx, `
		UPDATE jobs
		SET    state            = $3,
		       error_message    = $4,
		       finished_at      = now(),
		       lease_owner      = NULL,
		       lease_expires_at = NULL,
		       updated_at       = now()
		WHERE  id = $1 AND lease_owner = $2`,
		jobID, owner, state, message)
	if err != nil {
		return fmt.Errorf("scheduler: finish job %s: %w", jobID, err)
	}
	return nil
}

// reapMessage is what an operator reads on a job whose runner disappeared. It says what happened
// and what was done about it, because the row is the only account anyone will ever get.
const reapMessage = "the runner holding this job's lease stopped reporting; " +
	"the job was closed rather than re-run, and the next scheduled run will proceed normally"

// reap closes jobs whose lease expired, and the backup or verification rows they orphaned.
//
// This is the answer to a control plane killed mid-backup. The alternative — making the job
// claimable again — was considered and rejected in ADR-0025: the multipart upload was aborted, so
// no artifact exists to salvage, and re-running would mean a six-hour dump starting itself against
// a production server at the moment that server came back up. A missed backup is recoverable and
// visible. An unexpected one is an incident.
//
// The three statements run in one transaction so that a job is never closed while the backup row it
// points at still claims to be running: that pair of rows disagreeing is exactly the state this
// whole mechanism exists to prevent.
func reap(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("scheduler: begin the reap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		UPDATE jobs
		SET    state            = 'failed',
		       error_message    = $1,
		       finished_at      = now(),
		       lease_owner      = NULL,
		       lease_expires_at = NULL,
		       updated_at       = now()
		WHERE  state = 'running'
		  AND  lease_expires_at IS NOT NULL
		  AND  lease_expires_at < now()
		RETURNING id`, reapMessage)
	if err != nil {
		return 0, fmt.Errorf("scheduler: reap expired leases: %w", err)
	}

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scheduler: read a reaped job: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scheduler: reap expired leases: %w", err)
	}
	if len(ids) == 0 {
		return 0, tx.Commit(ctx)
	}

	// A backup left `running` by a killed process describes nothing: its multipart upload was never
	// completed, so there is no object behind it. Saying `failed` is the truth, and it is what stops
	// the estate view showing a backup in progress for the rest of the month.
	if _, err := tx.Exec(ctx, `
		UPDATE backups
		SET    state         = 'failed',
		       error_message = $2,
		       completed_at  = now(),
		       updated_at    = now()
		WHERE  job_id = ANY($1::uuid[]) AND state IN ('pending', 'running')`, ids, reapMessage); err != nil {
		return 0, fmt.Errorf("scheduler: close the backups orphaned by a reaped job: %w", err)
	}

	// An interrupted verification is INCONCLUSIVE, never FAILED. FAILED is reserved for evidence
	// about the artifact (ADR-0022), and a control plane that was killed is evidence about the
	// control plane.
	if _, err := tx.Exec(ctx, `
		UPDATE verifications
		SET    status        = 'inconclusive',
		       error_message = $2,
		       completed_at  = now(),
		       updated_at    = now()
		WHERE  job_id = ANY($1::uuid[]) AND status IN ('pending', 'running')`, ids, reapMessage); err != nil {
		return 0, fmt.Errorf("scheduler: close the verifications orphaned by a reaped job: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("scheduler: commit the reap: %w", err)
	}
	return int64(len(ids)), nil
}

// interval renders a duration for PostgreSQL's interval parser.
//
// Passing a time.Duration directly would send nanoseconds as an integer, which `now() + $2` reads
// as microseconds and silently makes a two-minute lease last two hours. Rendering seconds
// explicitly makes the unit part of the value.
func interval(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	return fmt.Sprintf("%f seconds", d.Seconds())
}
