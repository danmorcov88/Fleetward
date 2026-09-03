package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/objstore"
	"strconv"

	"github.com/danmorcov88/fleetward/internal/controlplane/audit"
)

// Retention removes the artifacts of managed backups that have outlived the retention their
// schedule declared, and nothing else.
//
// Everything in this file is written against one sentence: until this code existed, the worst a bug
// in Fleetward could do was report something untrue. Four properties follow from it, and each is
// enforced somewhere other than the query below, because a predicate in a query is a line somebody
// deletes while refactoring.
//
//   - **An observed backup can never be selected.** It is somebody else's file (ADR-0015). The
//     query filters on origin, the row's object_key is empty by construction, and the database
//     refuses the state transition outright — three independent barriers, and only the third
//     survives an author who has not read ADR-0015.
//   - **A backup with no expiry is never selected.** Every backup taken before this slice has one,
//     which is why upgrading deletes nothing (ADR-0031).
//   - **An instance's last good backup is never deleted** (ADR-0032).
//   - **The row is never deleted, only its bytes.** What is left is the audit record of what once
//     existed (ADR-0030).

// stampedExpiry turns the retention a run was given into the instant its artifact may be deleted.
//
// A retention of zero — or of anything else non-positive — returns nil, and nil is written to the
// column as NULL. NULL is not "expire immediately" and not "expire by default"; it is **never
// expires**, and the sweep does not select it.
//
// That is deliberate and it is the reason upgrading to this version deletes nothing. Every backup
// taken before retention existed has NULL here, as does every manual backup, because neither has a
// declared retention behind it and Fleetward does not invent one in order to delete something
// (ADR-0031).
func stampedExpiry(retentionDays int32, completedAt time.Time) *time.Time {
	if retentionDays <= 0 {
		return nil
	}
	expiry := completedAt.UTC().AddDate(0, 0, int(retentionDays))
	return &expiry
}

// RetentionPolicy is the configured shape of the sweep.
//
// It is a value rather than a dependency on the config package, so this service stays testable
// without an environment. Every field is a limit; none of them is a feature.
type RetentionPolicy struct {
	Enabled     bool
	Interval    time.Duration
	MinKeep     int
	MaxPerSweep int
}

// RetentionResult is what one sweep did, for the log line that is the only per-sweep record there
// is. There is no job row behind a sweep (ADR-0030), so this and the rows themselves are the whole
// account.
type RetentionResult struct {
	// Expired is how many backups moved from succeeded to expired.
	Expired int
	// ArtifactsDeleted is how many objects were removed from storage — including any left behind by
	// an earlier sweep that was interrupted between the two steps.
	ArtifactsDeleted int
	// BytesReclaimed is what those objects occupied.
	BytesReclaimed int64
	// Unreachable counts artifacts whose object could not be deleted this time. They stay queued
	// and the next sweep tries again; nothing is lost, and the count is here so a store that has
	// been failing all week is visible rather than silent.
	Unreachable int
}

// Empty reports whether the sweep did nothing, which is the ordinary case and not worth a log line.
func (r RetentionResult) Empty() bool {
	return r.Expired == 0 && r.ArtifactsDeleted == 0 && r.Unreachable == 0
}

// retentionCandidateColumns is the shape both the sweep and the preview read a candidate in.
//
// The two share it deliberately: a preview that answered a different question from the sweep would
// be worse than no preview, because it would be believed.
const retentionCandidateColumns = `c.id, c.instance_id, c.instance_name, c.completed_at,
       c.expires_at, c.size_bytes, c.bucket, c.object_key,
       c.floor_recent, c.floor_verified, c.busy`

// retentionCandidatesCTE is the one place that decides what retention may touch.
//
// It is a single named constant rather than a query written twice, because "the preview and the
// sweep disagree" is a defect that would be discovered by an operator watching an artifact vanish
// that the preview said was safe.
//
// $1 is the tenant, $2 is MinKeep.
const retentionCandidatesCTE = `
WITH owned AS (
    -- Everything retention could ever have an opinion about: a backup Fleetward took, that
    -- succeeded, and therefore has an artifact behind it. The origin filter is the first of three
    -- barriers protecting an observed backup, not the only one.
    SELECT b.id, b.instance_id, b.completed_at, b.expires_at, b.size_bytes, b.bucket, b.object_key
    FROM   backups b
    WHERE  b.tenant_id = $1 AND b.origin = 'managed' AND b.state = 'succeeded'
),
floor_recent AS (
    -- The most recent MinKeep successful backups of each instance, whatever their expiry says.
    --
    -- Computed over every owned backup rather than only the expired ones, which is the difference
    -- between a floor and an off-by-one: if an instance's three newest backups are all still young,
    -- the floor is already satisfied and the old ones are free to go.
    SELECT id FROM (
        SELECT o.id,
               row_number() OVER (PARTITION BY o.instance_id
                                  ORDER BY o.completed_at DESC NULLS LAST, o.id) AS rn
        FROM   owned o
    ) ranked
    WHERE rn <= $2
),
floor_verified AS (
    -- The most recent backup of each instance that was actually proven restorable.
    --
    -- This is the rule that makes the floor worth having. On a server whose backups have been
    -- succeeding and failing verification for a month, keeping only the newest would keep one known
    -- to be unrestorable and delete the last one proven good (ADR-0032).
    SELECT DISTINCT ON (o.instance_id) o.id
    FROM   owned o
    WHERE  (SELECT v.status FROM verifications v
            WHERE v.backup_id = o.id
            ORDER BY v.created_at DESC
            LIMIT 1) = 'verified'
    ORDER  BY o.instance_id, o.completed_at DESC NULLS LAST, o.id
),
busy AS (
    -- Backups something is reading right now. A verification downloads the artifact at run time, so
    -- deleting it underneath is a real race, and the lease machinery does not cover it: the lease is
    -- on a job row and this is a different row.
    --
    -- Both halves are needed. A queued verification has a job before it has a verifications row, so
    -- checking only the latter would leave the window open for exactly as long as the job sits
    -- pending.
    SELECT v.backup_id FROM verifications v
    WHERE  v.tenant_id = $1 AND v.status IN ('pending', 'running')
    UNION
    SELECT r.backup_id FROM restores r
    WHERE  r.tenant_id = $1 AND r.state IN ('pending', 'running')
    UNION
    SELECT (j.payload->>'backup_id')::uuid FROM jobs j
    WHERE  j.tenant_id = $1
      AND  j.kind IN ('verify', 'restore')
      AND  j.state IN ('pending', 'running')
      AND  j.payload->>'backup_id' IS NOT NULL
      AND  j.payload->>'backup_id' <> ''
),
candidates AS (
    SELECT o.id, o.instance_id, i.name AS instance_name, o.completed_at, o.expires_at,
           o.size_bytes, o.bucket, o.object_key,
           EXISTS (SELECT 1 FROM floor_recent   f WHERE f.id = o.id)        AS floor_recent,
           EXISTS (SELECT 1 FROM floor_verified f WHERE f.id = o.id)        AS floor_verified,
           EXISTS (SELECT 1 FROM busy           b WHERE b.backup_id = o.id) AS busy
    FROM   owned o
    JOIN   instances i ON i.id = o.instance_id
    -- An expiry that is NULL or in the future is not a candidate at all. NULL is the state every
    -- backup taken before this slice is in, and it is why upgrading deletes nothing (ADR-0031).
    WHERE  o.expires_at IS NOT NULL AND o.expires_at <= now()
)`

// SweepRetention expires what has outlived its retention and deletes the artifacts behind it.
//
// Two steps, in this order, and the order is the whole design (ADR-0030):
//
//  1. The row moves succeeded -> expired and that commits on its own. From that instant it no
//     longer claims a restorable artifact, verification refuses it, and the estate view reads
//     honestly.
//  2. The object is deleted, and only then is artifact_deleted_at written.
//
// A control plane killed between them leaves a row that is expired with its object still present.
// That state is self-reconciling: step 2 selects on exactly that predicate, so the next sweep — on
// any control plane, not necessarily the one that died — finishes the job, and deleting an object
// that is already gone is not an error.
//
// Nothing here holds a lease. Retention is idempotent in a way a backup is not: the state
// transition is its own guard, so a row is expired by exactly one of two concurrent sweeps and the
// other simply matches nothing.
func (s *Service) SweepRetention(ctx context.Context) (RetentionResult, error) {
	policy := s.retention
	var result RetentionResult
	if !policy.Enabled {
		return result, nil
	}
	if policy.MinKeep < 1 || policy.MaxPerSweep < 1 {
		// Belt and braces behind config.Validate. A zero floor reaching this far would mean
		// retention could delete an instance's last working backup, and the right response to that
		// is to refuse rather than to guess a value.
		return result, fmt.Errorf("%w: retention needs min_keep >= 1 and max_per_sweep >= 1, got %d and %d",
			ErrInvalidArgument, policy.MinKeep, policy.MaxPerSweep)
	}

	expired, err := s.expireOutlivedBackups(ctx, policy)
	if err != nil {
		return result, err
	}
	result.Expired = expired

	deleted, bytes, unreachable, err := s.deleteExpiredArtifacts(ctx, policy)
	result.ArtifactsDeleted = deleted
	result.BytesReclaimed = bytes
	result.Unreachable = unreachable
	if err != nil {
		return result, err
	}
	return result, nil
}

// expireOutlivedBackups moves eligible rows to `expired` and commits. It touches no object.
func (s *Service) expireOutlivedBackups(ctx context.Context, policy RetentionPolicy) (int, error) {
	rows, err := s.pool.Query(ctx, retentionCandidatesCTE+`
		UPDATE backups
		SET    state      = 'expired',
		       updated_at = now()
		WHERE  tenant_id = $1
		  AND  id IN (
		    SELECT c.id FROM candidates c
		    WHERE  NOT c.floor_recent AND NOT c.floor_verified AND NOT c.busy
		    ORDER  BY c.expires_at
		    LIMIT  $3
		)
		RETURNING id`,
		authn.Tenant(ctx), policy.MinKeep, policy.MaxPerSweep)
	if err != nil {
		return 0, fmt.Errorf("backup: expire outlived backups: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return count, fmt.Errorf("backup: read an expired backup: %w", err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, fmt.Errorf("backup: expire outlived backups: %w", err)
	}
	return count, nil
}

// pendingArtifact is one object still to remove: a row that is expired and whose bytes are still
// there. It is the queue an interrupted sweep leaves behind, and the queue a completed step 1 fills.
type pendingArtifact struct {
	backupID  string
	bucket    string
	objectKey string
	sizeBytes int64
}

// deleteExpiredArtifacts removes the objects behind expired rows, marking each only once it is gone.
//
// The order within each artifact is deliberate and is the opposite of step 1's: the object goes
// first and the row is marked afterwards. Marking first would produce a row claiming its artifact
// was deleted while the object was still there — an orphan nothing would ever look for again, which
// is the one leftover this design refuses to create.
func (s *Service) deleteExpiredArtifacts(ctx context.Context, policy RetentionPolicy) (deleted int, bytes int64, unreachable int, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, bucket, object_key, size_bytes
		FROM   backups
		WHERE  tenant_id = $1
		  AND  state = 'expired'
		  AND  artifact_deleted_at IS NULL
		  AND  object_key <> ''
		ORDER  BY updated_at
		LIMIT  $2`, authn.Tenant(ctx), policy.MaxPerSweep)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("backup: find artifacts to delete: %w", err)
	}

	var pending []pendingArtifact
	for rows.Next() {
		var p pendingArtifact
		if err := rows.Scan(&p.backupID, &p.bucket, &p.objectKey, &p.sizeBytes); err != nil {
			rows.Close()
			return 0, 0, 0, fmt.Errorf("backup: read an artifact to delete: %w", err)
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, 0, fmt.Errorf("backup: find artifacts to delete: %w", err)
	}

	for _, p := range pending {
		// A row naming a bucket this process is not configured for is left alone rather than
		// guessed at. The store deletes by key within its own bucket, so acting on such a row would
		// mean deleting the same key out of a different bucket — a plausible way to destroy the
		// wrong object entirely.
		if p.bucket != "" && p.bucket != s.store.Bucket() {
			unreachable++
			s.log.WarnContext(ctx, "an expired artifact is in a bucket this control plane is not configured for; left in place",
				slog.String("backup_id", p.backupID),
				slog.String("artifact_bucket", p.bucket),
				slog.String("configured_bucket", s.store.Bucket()))
			continue
		}

		if delErr := s.store.Delete(ctx, p.objectKey); delErr != nil && !errors.Is(delErr, objstore.ErrNotFound) {
			// Not fatal to the sweep. The row stays queued, the next sweep tries again, and nothing
			// has been lost — the artifact is still there and the row already says it is expired.
			unreachable++
			s.log.WarnContext(ctx, "could not delete an expired artifact; it stays queued for the next sweep",
				slog.String("backup_id", p.backupID),
				slog.String("object_key", p.objectKey),
				slog.String("error", delErr.Error()))
			continue
		}

		// The answer to "who deleted this artifact", written where the bytes actually go.
		//
		// It is recorded before the row is marked rather than after, for the same reason B5 expires
		// the row before deleting the object: of the two orders, this is the one whose failure mode
		// is a record of something that happened rather than silence about something that did.
		s.auditAutomatic(ctx, audit.Entry{
			Action:       "backup.expire",
			ResourceType: "backup",
			ResourceID:   p.backupID,
			Succeeded:    true,
			Details: map[string]string{
				"object_key":    p.objectKey,
				"bytes":         strconv.FormatInt(p.sizeBytes, 10),
				"authorized_by": "the retention sweep, which is not a person",
			},
		})

		if _, markErr := s.pool.Exec(ctx, `
			UPDATE backups
			SET    artifact_deleted_at = now(),
			       updated_at          = now()
			WHERE  id = $1 AND tenant_id = $2 AND artifact_deleted_at IS NULL`,
			p.backupID, authn.Tenant(ctx)); markErr != nil {
			// The object is gone and the row does not know it. Harmless and self-correcting: the
			// next sweep tries to delete an object that is already absent, which is not an error,
			// and marks the row then.
			return deleted, bytes, unreachable, fmt.Errorf("backup: record an artifact deletion: %w", markErr)
		}

		deleted++
		bytes += p.sizeBytes
		s.log.InfoContext(ctx, "deleted an expired backup artifact",
			slog.String("backup_id", p.backupID),
			slog.String("object_key", p.objectKey),
			slog.Int64("size_bytes", p.sizeBytes))
	}
	return deleted, bytes, unreachable, nil
}

// PreviewRetentionInput narrows a preview to one instance. Empty covers the estate.
type PreviewRetentionInput struct {
	InstanceID string
}

// PreviewRetention reports what the next sweep would do, without doing any of it.
//
// It exists because there is no job row per sweep to read afterwards (ADR-0030), and because
// retention is the only thing this product does that destroys something. An operator has to be able
// to see the answer before it is acted on rather than reconstruct it from a log.
//
// It reads through the same CTE the sweep does. A preview that answered a slightly different
// question would be worse than none, because it would be believed.
func (s *Service) PreviewRetention(ctx context.Context, in PreviewRetentionInput) (*fwv1.PreviewRetentionResponse, error) {
	policy := s.retention
	minKeep := policy.MinKeep
	if minKeep < 1 {
		minKeep = 1
	}

	args := []any{authn.Tenant(ctx), minKeep}
	filter := ""
	if in.InstanceID != "" {
		id, err := requireUUID("instance_id", in.InstanceID)
		if err != nil {
			return nil, err
		}
		args = append(args, id)
		filter = fmt.Sprintf(" WHERE c.instance_id = $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, retentionCandidatesCTE+`
		SELECT `+retentionCandidateColumns+`
		FROM   candidates c`+filter+`
		ORDER  BY c.expires_at, c.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("backup: preview retention: %w", err)
	}

	out := &fwv1.PreviewRetentionResponse{
		Policy: &fwv1.RetentionPolicy{
			Enabled:     policy.Enabled,
			Interval:    durationpb.New(policy.Interval),
			MinKeep:     int32(minKeep),            //nolint:gosec // bounded by configuration
			MaxPerSweep: int32(policy.MaxPerSweep), //nolint:gosec // bounded by configuration
		},
	}

	for rows.Next() {
		var (
			c                                fwv1.RetentionCandidate
			completedAt, expiresAt           *time.Time
			bucket, objectKey                string
			floorRecent, floorVerified, busy bool
		)
		if err := rows.Scan(&c.BackupId, &c.InstanceId, &c.InstanceName, &completedAt, &expiresAt,
			&c.SizeBytes, &bucket, &objectKey, &floorRecent, &floorVerified, &busy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("backup: read a retention candidate: %w", err)
		}
		c.CompletedAt = timestampOrNil(completedAt)
		c.ExpiresAt = timestampOrNil(expiresAt)

		switch reason := protectionReason(floorRecent, floorVerified, busy, minKeep); reason {
		case "":
			out.Expiring = append(out.Expiring, &c)
			out.ReclaimableBytes += c.GetSizeBytes()
		default:
			c.ProtectedReason = reason
			out.Protected = append(out.Protected, &c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: preview retention: %w", err)
	}

	pending, err := s.previewPendingDeletion(ctx, in.InstanceID)
	if err != nil {
		return nil, err
	}
	out.PendingDeletion = pending

	return out, nil
}

// protectionReason says, in a sentence a human can act on, why a backup past its expiry stays.
//
// It returns the empty string for a backup the sweep would remove. The floor is reported ahead of
// the concurrency guard because it is permanent while the guard is a matter of minutes, and the
// reader is usually asking "will this ever go" rather than "will this go right now".
func protectionReason(floorRecent, floorVerified, busy bool, minKeep int) string {
	switch {
	case floorRecent && floorVerified:
		return "kept: " + recentPhrase(minKeep) + ", and it is the most recent backup proven restorable"
	case floorRecent:
		return "kept: " + recentPhrase(minKeep) + ", which retention never deletes however old it is"
	case floorVerified:
		return "kept: it is the most recent backup of this instance proven restorable, and deleting " +
			"the last proof is worse than keeping one old artifact"
	case busy:
		return "not deleted yet: something is verifying or restoring from it right now. It becomes " +
			"eligible again as soon as that finishes"
	default:
		return ""
	}
}

// recentPhrase renders the floor's first rule as a clause that reads as English at either width.
//
// The two cases are genuinely different sentences rather than one sentence with a number in it:
// with a floor of one the backup *is* the instance's most recent, and with a floor of three it is
// merely among them. Splicing a count into a single template produced "it is among this instance's
// most recent successful backup", which the walk found in the first preview it printed.
func recentPhrase(minKeep int) string {
	if minKeep <= 1 {
		return "it is this instance's most recent successful backup"
	}
	return fmt.Sprintf("it is among this instance's %d most recent successful backups", minKeep)
}

// previewPendingDeletion lists artifacts already expired whose objects are still there — what a
// sweep interrupted between its two steps leaves behind, and what the next sweep will finish.
func (s *Service) previewPendingDeletion(ctx context.Context, instanceID string) ([]*fwv1.RetentionCandidate, error) {
	args := []any{authn.Tenant(ctx)}
	filter := ""
	if instanceID != "" {
		id, err := requireUUID("instance_id", instanceID)
		if err != nil {
			return nil, err
		}
		args = append(args, id)
		filter = fmt.Sprintf(" AND b.instance_id = $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, `
		SELECT b.id, b.instance_id, i.name, b.completed_at, b.expires_at, b.size_bytes
		FROM   backups b
		JOIN   instances i ON i.id = b.instance_id
		WHERE  b.tenant_id = $1
		  AND  b.state = 'expired'
		  AND  b.artifact_deleted_at IS NULL
		  AND  b.object_key <> ''`+filter+`
		ORDER  BY b.updated_at`, args...)
	if err != nil {
		return nil, fmt.Errorf("backup: list artifacts awaiting deletion: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.RetentionCandidate
	for rows.Next() {
		var (
			c                      fwv1.RetentionCandidate
			completedAt, expiresAt *time.Time
		)
		if err := rows.Scan(&c.BackupId, &c.InstanceId, &c.InstanceName, &completedAt, &expiresAt,
			&c.SizeBytes); err != nil {
			return nil, fmt.Errorf("backup: read an artifact awaiting deletion: %w", err)
		}
		c.CompletedAt = timestampOrNil(completedAt)
		c.ExpiresAt = timestampOrNil(expiresAt)
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: list artifacts awaiting deletion: %w", err)
	}
	return out, nil
}
