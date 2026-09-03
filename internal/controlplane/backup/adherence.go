package backup

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

const (
	// dstAllowance widens the window for a backup whose finish time could not be converted to UTC
	// with the offset in force on the day it was taken.
	//
	// An engine that records local time with no offset — which is most of them — can only be read
	// exactly by an instance that will name its own time zone. Where it will not, the error is
	// bounded: exactly one daylight-saving transition, which is one hour everywhere that observes
	// one. Widening by that much turns a wrong answer into a slightly weaker one, and the answer
	// says which it is (ADR-0027).
	dstAllowance = time.Hour

	// adherenceSearchLimit bounds how far back the previous-occurrence search walks. An expectation
	// that has not fired in a year is not an expectation anybody is watching.
	adherenceSearchLimit = 400 * 24 * time.Hour
)

// GetAdherenceInput filters the estate the question is asked about.
type GetAdherenceInput struct {
	InstanceID    string
	EnvironmentID string
	ProblemsOnly  bool
}

// GetBackupAdherence answers the question this product exists for: did every server's backup run
// when it was supposed to, and did it succeed.
//
// Two properties are deliberate.
//
// It counts backups of both origins. Adherence does not care who took the backup — a nightly dump
// written by somebody's cron job since 2019 satisfies a window exactly as a backup Fleetward took
// does — and that is what makes this answerable on the day Fleetward is installed, against an
// estate nothing has been changed on (ADR-0015).
//
// And it is computed on read. No verdict is stored anywhere, so there is no table that can be stale,
// no reconciliation to get wrong after a schedule is edited, and nothing to backfill when an
// expectation changes. The alert rule that will read this later reads the same computation rather
// than a row it would have to trust.
func (s *Service) GetBackupAdherence(ctx context.Context, in GetAdherenceInput) ([]*fwv1.InstanceAdherence, error) {
	expectations, err := s.loadExpectations(ctx, in)
	if err != nil {
		return nil, err
	}
	if len(expectations) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	for _, e := range expectations {
		// Nothing declared is the ordinary case, not a failure to parse. Reporting the empty
		// expression's parse error as a caveat describes a misconfiguration that does not exist,
		// and on an estate where most instances have no declaration yet that is most of the rows
		// carrying a warning about a schedule nobody wrote. It reads as NOT_DECLARED and says
		// nothing further; the estate view renders that state in words of its own.
		if e.cron == "" {
			e.state = fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED
			continue
		}
		if err := e.evaluateWindow(now); err != nil {
			// A cron expression that stopped parsing — because a time zone left the system
			// database, say — must not take the whole estate's answer down with it. This one was
			// declared and is broken, which is worth saying out loud.
			e.state = fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED
			e.caveats = append(e.caveats, "the declared schedule could not be evaluated: "+err.Error())
		}
	}

	if err := s.attachWindowBackups(ctx, expectations); err != nil {
		return nil, err
	}
	if err := s.attachLatestBackups(ctx, expectations); err != nil {
		return nil, err
	}
	if err := s.attachVerifications(ctx, expectations); err != nil {
		return nil, err
	}

	out := make([]*fwv1.InstanceAdherence, 0, len(expectations))
	for _, e := range expectations {
		answer := e.answer()
		if in.ProblemsOnly && answer.GetState() == fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT {
			continue
		}
		out = append(out, answer)
	}
	return out, nil
}

// expectation is one instance and what was declared for it, carried through the evaluation.
type expectation struct {
	instanceID    string
	instanceName  string
	environmentID string
	engineType    string

	cron         string
	timezone     string
	graceMinutes int32

	// expectedBy is the most recent instant a backup was supposed to have happened by, whose grace
	// period has already run out. deadline is that instant plus the grace.
	expectedBy time.Time
	deadline   time.Time

	// candidates are the backups that completed near enough to the window to be considered, newest
	// first. satisfied is the one that actually settles the answer.
	candidates []*fwv1.Backup
	satisfied  *fwv1.Backup
	latest     *fwv1.Backup

	state   fwv1.AdherenceState
	caveats []string
}

// evaluateWindow computes which occurrence of the declared schedule is currently being judged.
//
// The occurrence chosen is the most recent one whose grace period has already expired, and that is
// the point: an instance expected to back up at 02:00 with two hours of grace must not be reported
// as missing a backup at 02:30, while the backup is still running. Until the grace runs out there
// is nothing to answer yet, so the previous occurrence stays the one under judgement.
func (e *expectation) evaluateWindow(now time.Time) error {
	grace := time.Duration(e.graceMinutes) * time.Minute
	occurrence, err := previousRun(e.cron, e.timezone, now.Add(-grace))
	if err != nil {
		return err
	}
	e.expectedBy = occurrence
	e.deadline = occurrence.Add(grace)
	return nil
}

// answer turns everything gathered into the contract message.
func (e *expectation) answer() *fwv1.InstanceAdherence {
	out := &fwv1.InstanceAdherence{
		InstanceId:           e.instanceID,
		InstanceName:         e.instanceName,
		EnvironmentId:        e.environmentID,
		EngineType:           e.engineType,
		ExpectedCron:         e.cron,
		Timezone:             e.timezone,
		ExpectedGraceMinutes: e.graceMinutes,
		LatestBackup:         e.latest,
	}
	if e.state == fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED {
		out.State = e.state
		out.Caveats = e.caveats
		return out
	}

	out.ExpectedBy = timestamppb.New(e.expectedBy)
	out.Deadline = timestamppb.New(e.deadline)
	out.SatisfiedBy = e.satisfied
	out.State = e.state
	out.Caveats = append(append([]string{}, e.caveats...), caveatsFor(e.satisfied)...)
	return out
}

// caveatsFor says, in a sentence a human can act on, what weakens an answer that rests on evidence
// Fleetward did not produce itself.
//
// Every one of these is a fact the plugin declared about its source rather than something core
// inferred. An answer resting on a backup Fleetward took has none of them, and says nothing.
func caveatsFor(b *fwv1.Backup) []string {
	evidence := b.GetEvidence()
	if evidence == nil {
		return nil
	}

	var out []string
	source := evidence.GetSourceDescription()
	if source == "" {
		source = "the source this was read from"
	}
	if !evidence.GetReportsOutcome() {
		out = append(out, fmt.Sprintf(
			"%s records that a backup arrived and cannot say whether it completed successfully",
			source))
	}
	if !evidence.GetIdentityIsEngineAssigned() {
		out = append(out, fmt.Sprintf(
			"%s assigns no identity of its own, so a backup that is moved or renamed appears as a "+
				"new one", source))
	}
	if evidence.GetCompletedAtIsApproximate() {
		out = append(out, fmt.Sprintf(
			"the finish time from %s may be out by up to an hour across a daylight-saving change, "+
				"so the window was widened by that much", source))
	}
	return out
}

// loadExpectations reads every instance in scope together with what was declared for it.
//
// An instance with no declaration is reported as NOT_DECLARED rather than omitted: "nobody has said
// what this server's backups should look like" is a finding on an estate of fifty, and dropping the
// row would hide it.
func (s *Service) loadExpectations(ctx context.Context, in GetAdherenceInput) ([]*expectation, error) {
	args := []any{s.tenantID}
	filters := ""
	if in.InstanceID != "" {
		id, err := requireUUID("instance_id", in.InstanceID)
		if err != nil {
			return nil, err
		}
		args = append(args, id)
		filters += fmt.Sprintf(" AND i.id = $%d", len(args))
	}
	if in.EnvironmentID != "" {
		id, err := requireUUID("environment_id", in.EnvironmentID)
		if err != nil {
			return nil, err
		}
		args = append(args, id)
		filters += fmt.Sprintf(" AND i.environment_id = $%d", len(args))
	}

	// The lateral picks one declaration per instance. More than one enabled schedule carrying an
	// expectation is a configuration nobody meant, and the newest is the one they were editing.
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.name, i.environment_id, i.engine_type,
		       COALESCE(e.expected_cron, ''), COALESCE(e.timezone, 'UTC'),
		       COALESCE(e.expected_grace_minutes, 0)
		FROM   instances AS i
		LEFT   JOIN LATERAL (
		           SELECT expected_cron, timezone, expected_grace_minutes
		           FROM   schedules
		           WHERE  instance_id = i.id AND is_enabled AND expected_cron <> ''
		           ORDER  BY updated_at DESC
		           LIMIT  1
		       ) AS e ON TRUE
		WHERE  i.tenant_id = $1 AND i.is_active`+filters+`
		ORDER  BY i.name`, args...)
	if err != nil {
		return nil, fmt.Errorf("backup: read backup expectations: %w", err)
	}
	defer rows.Close()

	var out []*expectation
	for rows.Next() {
		e := &expectation{}
		if err := rows.Scan(&e.instanceID, &e.instanceName, &e.environmentID, &e.engineType,
			&e.cron, &e.timezone, &e.graceMinutes); err != nil {
			return nil, fmt.Errorf("backup: read a backup expectation: %w", err)
		}
		if e.cron == "" {
			e.state = fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: read backup expectations: %w", err)
	}
	return out, nil
}

// attachWindowBackups finds, in one query, the backups that fall near each instance's window, and
// decides which of them settles the answer.
//
// One query rather than one per instance: this is the estate view's question, asked about fifty
// servers on an interval, and fifty round trips per refresh is how a dashboard becomes the reason
// somebody turns the dashboard off.
func (s *Service) attachWindowBackups(ctx context.Context, expectations []*expectation) error {
	byID := map[string]*expectation{}
	args := []any{s.tenantID}
	var values []string
	for _, e := range expectations {
		if e.state == fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED {
			continue
		}
		byID[e.instanceID] = e
		// Widened by the daylight-saving allowance on both sides. Whether a record is actually
		// entitled to that extra hour is decided per record below, because only some of them are.
		args = append(args, e.instanceID, e.expectedBy.Add(-dstAllowance), e.deadline.Add(dstAllowance))
		values = append(values, fmt.Sprintf("($%d::uuid, $%d::timestamptz, $%d::timestamptz)",
			len(args)-2, len(args)-1, len(args)))
	}
	if len(values) == 0 {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		WITH windows (instance_id, lo, hi) AS (VALUES `+strings.Join(values, ", ")+`)
		SELECT `+prefixed(backupColumns, "b.")+`
		FROM   backups AS b
		JOIN   windows AS w ON w.instance_id = b.instance_id
		WHERE  b.tenant_id = $1
		  AND  b.completed_at >= w.lo
		  AND  b.completed_at <= w.hi
		ORDER  BY b.instance_id, b.completed_at DESC`, args...)
	if err != nil {
		return fmt.Errorf("backup: read backups in the expected windows: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		b, err := s.scanBackup(rows, nil)
		if err != nil {
			return fmt.Errorf("backup: read a backup in an expected window: %w", err)
		}
		if e := byID[b.GetInstanceId()]; e != nil {
			e.candidates = append(e.candidates, b)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: read backups in the expected windows: %w", err)
	}

	for _, e := range byID {
		e.decide()
	}
	return nil
}

// decide picks the backup that settles this window, and the state it settles it as.
//
// The order of preference is deliberate. A window containing a failed backup and a successful one
// is adherent: the failure was retried and the retry worked, which is the estate behaving. A window
// containing only evidence that cannot report an outcome is UNPROVEN rather than ADHERENT, because
// a file that arrived is not a backup that worked, and rendering those as the same green tick is
// the false confidence this product exists to eliminate (ADR-0015).
func (e *expectation) decide() {
	rank := func(b *fwv1.Backup) int {
		switch b.GetState() {
		case fwv1.BackupState_BACKUP_STATE_SUCCEEDED:
			return 0
		case fwv1.BackupState_BACKUP_STATE_UNKNOWN:
			return 1
		case fwv1.BackupState_BACKUP_STATE_FAILED:
			return 2
		default:
			return 3
		}
	}

	best := -1
	for _, b := range e.candidates {
		if !e.inWindow(b) || rank(b) > 2 {
			continue
		}
		if e.satisfied == nil || rank(b) < best {
			e.satisfied, best = b, rank(b)
		}
	}

	switch {
	case e.satisfied == nil:
		e.state = fwv1.AdherenceState_ADHERENCE_STATE_MISSED
	case best == 0:
		e.state = fwv1.AdherenceState_ADHERENCE_STATE_ADHERENT
	case best == 1:
		e.state = fwv1.AdherenceState_ADHERENCE_STATE_UNPROVEN
	default:
		e.state = fwv1.AdherenceState_ADHERENCE_STATE_FAILED
	}
}

// inWindow decides whether a backup is close enough to count, and it is where the daylight-saving
// allowance is actually spent. A backup whose finish time is exact gets the window as declared; one
// whose finish time the plugin could not pin down gets the extra hour it is entitled to and no more.
func (e *expectation) inWindow(b *fwv1.Backup) bool {
	completed := b.GetCompletedAt().AsTime()
	lo, hi := e.expectedBy, e.deadline
	if b.GetEvidence().GetCompletedAtIsApproximate() {
		lo, hi = lo.Add(-dstAllowance), hi.Add(dstAllowance)
	}
	return !completed.Before(lo) && !completed.After(hi)
}

// attachLatestBackups reads the most recent backup of any kind per instance.
//
// It is populated even for an instance that missed its window, because "the last one was nine days
// ago" is the useful half of a miss, and "there has never been one" is a different problem entirely.
func (s *Service) attachLatestBackups(ctx context.Context, expectations []*expectation) error {
	byID := make(map[string]*expectation, len(expectations))
	ids := make([]string, 0, len(expectations))
	for _, e := range expectations {
		byID[e.instanceID] = e
		ids = append(ids, e.instanceID)
	}
	if len(ids) == 0 {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (b.instance_id) `+prefixed(backupColumns, "b.")+`
		FROM   backups AS b
		WHERE  b.tenant_id = $1 AND b.instance_id = ANY($2) AND b.completed_at IS NOT NULL
		ORDER  BY b.instance_id, b.completed_at DESC`, s.tenantID, ids)
	if err != nil {
		return fmt.Errorf("backup: read the latest backup per instance: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		b, err := s.scanBackup(rows, nil)
		if err != nil {
			return fmt.Errorf("backup: read the latest backup of an instance: %w", err)
		}
		if e := byID[b.GetInstanceId()]; e != nil {
			e.latest = b
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: read the latest backup per instance: %w", err)
	}
	return nil
}

// attachVerifications fills in the latest verification of every managed backup this answer carries.
//
// It exists because the estate view needs the second half of the two-part status for fifty
// instances at once, and until this slice the only way to get a verification was GetBackup, one
// backup at a time. Fifty round trips per refresh, every thirty seconds, is not a dashboard.
//
// Two things it deliberately does not do:
//
// Observed backups are skipped rather than queried and found empty. A backup Fleetward did not take
// carries no manifest and can never be verified, so "no verification row" means something different
// for it than for a managed backup — the first is a permanent fact, the second is a gap
// (ADR-0015). Skipping them keeps that distinction in the origin, where it belongs, rather than
// inviting a reader to infer it from an absence.
//
// And it does not decode per-check results. A verification's checks are its detail, they can be
// large, and no column of the estate view renders them — GetBackup is where a reader goes for the
// report on one backup. What comes back here is the verdict and when it was reached.
func (s *Service) attachVerifications(ctx context.Context, expectations []*expectation) error {
	byBackup := map[string][]*fwv1.Backup{}
	for _, e := range expectations {
		for _, b := range []*fwv1.Backup{e.satisfied, e.latest} {
			if b == nil || b.GetOrigin() != fwv1.BackupOrigin_BACKUP_ORIGIN_MANAGED {
				continue
			}
			byBackup[b.GetId()] = append(byBackup[b.GetId()], b)
		}
	}
	if len(byBackup) == 0 {
		return nil
	}

	ids := make([]string, 0, len(byBackup))
	for id := range byBackup {
		ids = append(ids, id)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (backup_id)
		       backup_id, id, status, report, error_message, started_at, completed_at, duration_ms
		FROM   verifications
		WHERE  tenant_id = $1 AND backup_id = ANY($2)
		ORDER  BY backup_id, created_at DESC`, s.tenantID, ids)
	if err != nil {
		return fmt.Errorf("backup: read the latest verification per backup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			backupID    string
			status      string
			startedAt   *time.Time
			completedAt *time.Time
			durationMS  int64
			v           = &fwv1.Verification{}
		)
		if err := rows.Scan(&backupID, &v.Id, &status, &v.Report, &v.ErrorMessage,
			&startedAt, &completedAt, &durationMS); err != nil {
			return fmt.Errorf("backup: read a verification: %w", err)
		}
		v.BackupId = backupID
		v.Status = parseVerificationStatus(status)
		v.Duration = durationpb.New(time.Duration(durationMS) * time.Millisecond)
		v.StartedAt = timestampOrNil(startedAt)
		v.CompletedAt = timestampOrNil(completedAt)

		// The same backup can be both the one that satisfied the window and the latest one, and
		// they are the same pointer when it is. Assigning to every target keeps that harmless.
		for _, b := range byBackup[backupID] {
			b.Verification = v
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: read the latest verification per backup: %w", err)
	}
	return nil
}

// previousRun computes the most recent firing of a cron expression at or before `at`.
//
// robfig/cron answers "when next", never "when last", so this walks forward from a start far enough
// back to contain an occurrence. The window expands rather than being fixed: a five-minute cron
// searched over a year would be a hundred thousand iterations, and a monthly one searched over a
// day would find nothing. Starting small and doubling means the number of steps is bounded by how
// often the expression actually fires, whatever that is.
//
// The timezone handling is the same as the scheduler's nextRun and for the same reason: a DBA
// writing "0 2 * * *" for a Bucharest server means 02:00 there, which is a different UTC instant in
// summer than in winter. This is the mirror of that computation rather than a second copy of it —
// the scheduler asks when work is due, and this asks what should already have happened.
func previousRun(expr, timezone string, at time.Time) (time.Time, error) {
	spec, err := cron.ParseStandard(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a valid cron expression: %w", expr, err)
	}
	loc, err := time.LoadLocation(orUTC(timezone))
	if err != nil {
		return time.Time{}, fmt.Errorf("unknown timezone %q: %w", timezone, err)
	}

	local := at.In(loc)
	for window := time.Hour; window <= adherenceSearchLimit; window *= 4 {
		cursor := local.Add(-window)
		if spec.Next(cursor).After(local) {
			continue // nothing fires inside this window; widen it
		}
		last := cursor
		for {
			next := spec.Next(last)
			if next.After(local) {
				return last.UTC(), nil
			}
			last = next
		}
	}
	return time.Time{}, fmt.Errorf("%q has not fired in the last %d days", expr,
		int(adherenceSearchLimit.Hours()/24))
}

func orUTC(timezone string) string {
	if strings.TrimSpace(timezone) == "" {
		return "UTC"
	}
	return timezone
}
