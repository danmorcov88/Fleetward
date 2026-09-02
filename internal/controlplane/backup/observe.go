package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/inventory"
	"github.com/danmorcov88/fleetward/internal/telemetry"
)

const (
	// observeOverlap is how far back before the watermark every poll re-reads.
	//
	// It exists because evidence does not always arrive in the order it was created: an engine's
	// history row is written when a backup finishes, a file's timestamp is when it was last
	// written, and neither is guaranteed monotonic across a clock adjustment or a slow write. The
	// overlap is free, and that is the point of ADR-0027: re-reading a record Fleetward already has
	// is an upsert onto the same row, so the cost of being generous here is one wasted comparison
	// and the cost of being mean is a backup nobody ever sees.
	observeOverlap = 6 * time.Hour

	// observeHorizon bounds the very first poll of an instance.
	//
	// An instance with no observed backups yet has no watermark, and asking an engine that has been
	// up for six years for its entire backup history is the query this whole design exists to avoid
	// running against a production server. Thirty days is enough to answer every adherence question
	// there is, since the longest window anybody declares is monthly.
	observeHorizon = 30 * 24 * time.Hour

	// observePageLimit is what each call to the plugin asks for, and observeMaxPages caps how many
	// of those one poll makes. Together they bound the work a single poll can do at 10,000 records:
	// beyond that the next poll continues from a watermark that has moved, so a long backlog is
	// caught up over several runs rather than in one that never finishes.
	observePageLimit = 500
	observeMaxPages  = 20

	// observeTimeout bounds one whole poll, across every page.
	observeTimeout = 10 * time.Minute
)

// ObserveInput names the instance to read backup history from.
type ObserveInput struct {
	InstanceID string
}

// ObserveResult is what one poll found.
type ObserveResult struct {
	// Discovered counts records Fleetward had never seen.
	Discovered int32
	// Updated counts records that matched a backup already known — including the ones that matched
	// a backup Fleetward itself took, which is how the two origins converge instead of duplicating
	// (ADR-0027).
	Updated int32
	// Watermark is the finish time of the newest evidence read, which is where the next poll
	// resumes from.
	Watermark time.Time
}

// ObserveBackupHistory reads the engine's own record of backups Fleetward did not take, and records
// what it finds (ADR-0015).
//
// This is the RPC that makes Fleetward adoptable on an estate that already exists. It changes
// nothing on the instance: the plugin reads whatever evidence the engine keeps, and core writes
// rows about it. No artifact is fetched, moved, or deleted, and nothing is written to the monitored
// server.
//
// It is synchronous, unlike RunBackup. A poll is bounded work measured in seconds, and the caller —
// a scheduled job holding a lease, or a human at a CLI — wants to know what it found.
func (s *Service) ObserveBackupHistory(ctx context.Context, in ObserveInput) (ObserveResult, error) {
	conn, caps, err := s.prepareObservation(ctx, in.InstanceID)
	if err != nil {
		return ObserveResult{}, err
	}

	client, _, err := s.plugins.Client(conn.EngineType)
	if err != nil {
		return ObserveResult{}, fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, conn.EngineType, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, observeTimeout)
	defer cancel()

	watermark, err := s.observationWatermark(runCtx, conn.InstanceID)
	if err != nil {
		return ObserveResult{}, err
	}

	log := s.log.With(
		slog.String("instance_id", conn.InstanceID),
		slog.String("engine_type", conn.EngineType),
		slog.String("request_id", telemetry.RequestIDFrom(ctx)))
	log.DebugContext(runCtx, "reading backup history",
		slog.Time("since", watermark),
		slog.String("source", caps.GetBackupHistory().GetSourceDescription()))

	evidence := &fwv1.ObservedEvidence{
		SourceDescription:        caps.GetBackupHistory().GetSourceDescription(),
		ReportsOutcome:           caps.GetBackupHistory().GetReportsOutcome(),
		IdentityIsEngineAssigned: caps.GetBackupHistory().GetIdentityIsEngineAssigned(),
	}

	var (
		out       ObserveResult
		pageToken string
	)
	for page := 0; page < observeMaxPages; page++ {
		resp, err := client.ListBackupHistory(runCtx, &fwv1.ListBackupHistoryRequest{
			Connection:  conn.Ref,
			Credentials: conn.Credentials,
			Since:       timestamppb.New(watermark),
			Limit:       observePageLimit,
			PageToken:   pageToken,
			Timeout:     durationpb.New(observeTimeout),
		})
		if err != nil {
			return out, pluginError("list backup history", err)
		}

		for _, observed := range resp.GetBackups() {
			inserted, err := s.upsertObserved(runCtx, conn.InstanceID, observed, evidence)
			if err != nil {
				return out, err
			}
			if inserted {
				out.Discovered++
			} else {
				out.Updated++
			}
			if finished := observed.GetFinishedAt().AsTime(); finished.After(out.Watermark) {
				out.Watermark = finished
			}
		}

		pageToken = resp.GetNextPageToken()
		if pageToken == "" {
			break
		}
	}

	log.InfoContext(runCtx, "read backup history",
		slog.Int("discovered", int(out.Discovered)),
		slog.Int("updated", int(out.Updated)))
	return out, nil
}

// prepareObservation resolves the instance and refuses, before anything is read, a plugin that
// cannot see backup history or an instance that is missing what the plugin needs to see it.
//
// Both refusals happen where a human is asking. Core learns nothing about the engine doing the
// refusing: it reads two flags the plugin published (ADR-0015, ADR-0026).
func (s *Service) prepareObservation(ctx context.Context, instanceID string) (*inventory.Connection, *fwv1.Capabilities, error) {
	conn, err := s.resolver.ResolveConnection(ctx, instanceID)
	if err != nil {
		return nil, nil, err
	}

	_, caps, err := s.plugins.Client(conn.EngineType)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrEngineUnavailable, conn.EngineType, err)
	}

	history := caps.GetBackupHistory()
	if !history.GetSupported() {
		return nil, nil, fmt.Errorf(
			"%w: the plugin for %s cannot see backups it did not take, so there is no history to "+
				"read on this instance", ErrUnsupported, conn.EngineType)
	}
	if history.GetRequiresSharedDirectory() {
		share := conn.Credentials.GetSharedDirectory()
		if share.GetLocalPath() == "" {
			return nil, nil, fmt.Errorf(
				"%w: observing this instance's backups means reading the directory they are "+
					"written to (%s), and this instance has none configured: set the connection's "+
					"engine_path to the directory the backups are written to, and local_path to "+
					"where this control plane reaches the same directory",
				ErrInvalidArgument, history.GetSourceDescription())
		}
	}
	return conn, caps, nil
}

// observationWatermark decides where this poll starts reading.
//
// It is derived rather than stored, and that is the whole design. A column holding "where we got
// to" is a second source of truth that can disagree with the rows it describes: lost in a restore
// of the metadata database, stale after a poll that half succeeded, and wrong forever if anything
// ever writes it out of order. Reading it back off the rows themselves cannot drift, self-heals
// after a missed poll, and survives a restore, because the rows are the answer.
//
// The overlap makes re-reading deliberate rather than accidental, which is safe precisely because
// an identity assigned by the engine turns a repeated record into an upsert (ADR-0027).
func (s *Service) observationWatermark(ctx context.Context, instanceID string) (time.Time, error) {
	var newest *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(completed_at)
		FROM   backups
		WHERE  tenant_id = $1 AND instance_id = $2
		  AND  (origin = $3 OR external_id IS NOT NULL)`,
		s.tenantID, instanceID, originObserved).Scan(&newest)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup: read the observation watermark: %w", err)
	}
	if newest == nil {
		return time.Now().Add(-observeHorizon).UTC(), nil
	}
	return newest.Add(-observeOverlap).UTC(), nil
}

// upsertObserved records one piece of evidence, and reports whether it was new.
//
// The conflict target is the engine's own identity for the backup, which is what makes a poll
// idempotent: the same backup read on every poll for a year is one row. The DO UPDATE deliberately
// touches only rows that are already observed, so a managed backup whose engine recorded it in its
// own history — every SQL Server backup Fleetward takes — is matched and left exactly as it is,
// rather than being rewritten as somebody else's backup and losing its manifest (ADR-0027).
func (s *Service) upsertObserved(
	ctx context.Context,
	instanceID string,
	observed *fwv1.ObservedBackup,
	evidence *fwv1.ObservedEvidence,
) (inserted bool, err error) {
	externalID := observed.GetExternalId()
	if externalID == "" {
		// The contract requires it, and without it every poll would insert the same backup again.
		// A plugin defect is worth failing loudly for: silently dropping the record would report a
		// gap in somebody's backups that is really a gap in ours.
		return false, fmt.Errorf(
			"%w: the plugin reported a backup with no external_id, which the contract requires to "+
				"be a stable identity for it", ErrPluginFailed)
	}

	perRecord := &fwv1.ObservedEvidence{
		SourceDescription:        evidence.GetSourceDescription(),
		ReportsOutcome:           evidence.GetReportsOutcome(),
		IdentityIsEngineAssigned: evidence.GetIdentityIsEngineAssigned(),
		CompletedAtIsApproximate: observed.GetFinishedAtIsApproximate(),
	}
	encoded, err := protojson.Marshal(perRecord)
	if err != nil {
		return false, fmt.Errorf("backup: encode observation evidence: %w", err)
	}

	started := nullableTime(observed.GetStartedAt())
	completed := nullableTime(observed.GetFinishedAt())
	var durationMS int64
	if started != nil && completed != nil && completed.After(*started) {
		durationMS = completed.Sub(*started).Milliseconds()
	}

	// A record is inserted when xmax is zero: PostgreSQL leaves it so on a fresh tuple and sets it
	// on one an ON CONFLICT update rewrote. No rows at all means the conflict landed on a managed
	// backup, which the WHERE clause protects — matched, and correctly left alone.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO backups (tenant_id, instance_id, origin, external_id, method_id, state,
		                     size_bytes, external_location, evidence, started_at, completed_at,
		                     duration_ms, observed_at, triggered_manually)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now(), FALSE)
		ON CONFLICT (instance_id, external_id) WHERE external_id IS NOT NULL
		DO UPDATE SET state             = EXCLUDED.state,
		              method_id         = EXCLUDED.method_id,
		              size_bytes        = EXCLUDED.size_bytes,
		              external_location = EXCLUDED.external_location,
		              evidence          = EXCLUDED.evidence,
		              started_at        = EXCLUDED.started_at,
		              completed_at      = EXCLUDED.completed_at,
		              duration_ms       = EXCLUDED.duration_ms,
		              observed_at       = now(),
		              updated_at        = now()
		WHERE backups.origin = $3
		RETURNING (xmax = 0)`,
		s.tenantID, instanceID, originObserved, externalID,
		observedMethod(observed), observedState(observed),
		observed.GetSizeBytes(), observed.GetLocation(), encoded,
		started, completed, durationMS).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		// The engine's history describes a backup Fleetward took. There is nothing to write: the
		// managed row already says everything this evidence would have, and more.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backup: record an observed backup: %w", err)
	}
	return inserted, nil
}

// observedState maps what the evidence said into the state column.
//
// UNKNOWN is not rounded to either neighbour. A directory listing proves a file arrived and proves
// nothing about whether the dump that wrote it finished, and reporting that as success is exactly
// the false confidence this product exists to eliminate (ADR-0015).
func observedState(observed *fwv1.ObservedBackup) string {
	switch observed.GetOutcome() {
	case fwv1.ObservedOutcome_OBSERVED_OUTCOME_SUCCEEDED:
		return "succeeded"
	case fwv1.ObservedOutcome_OBSERVED_OUTCOME_FAILED:
		return "failed"
	case fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNKNOWN, fwv1.ObservedOutcome_OBSERVED_OUTCOME_UNSPECIFIED:
		return "unknown"
	default:
		return "unknown"
	}
}

// observedMethod keeps the NOT NULL method_id column populated with the engine's own word for what
// kind of backup this was. Core does not interpret it.
func observedMethod(observed *fwv1.ObservedBackup) string {
	if m := observed.GetMethod(); m != "" {
		return m
	}
	return "unknown"
}
