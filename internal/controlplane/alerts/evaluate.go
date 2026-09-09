package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
)

// The rule kinds, spelled as the CHECK constraint spells them.
//
// This list is the whole vocabulary of what Fleetward can be told to watch. It grows by migration,
// it maps one-to-one onto evaluators, and it contains no engine name — nothing in this package
// reads `instances.engine_type`, and a grep for an engine here should find nothing
// (CLAUDE.md §4.1).
const (
	KindInstanceDown       = "instance_down"
	KindVerificationFailed = "verification_failed"
	KindBackupFailed       = "backup_failed"
	KindBackupMissing      = "backup_missing"
	KindRetentionBlocked   = "retention_blocked"

	// Declared in the schema, deliberately without an evaluator. All three want metrics nothing
	// collects yet, and CreateAlertRule refuses them rather than storing a rule that would never
	// fire.
	KindStorageThreshold = "storage_threshold"
	KindReplicationLag   = "replication_lag"
	KindCustomPromQL     = "custom_promql"
)

// evaluatedKinds is what one pass covers, and therefore what one pass may resolve.
//
// The two are the same list on purpose. Resolution works by absence — an open alert whose condition
// was not found this pass is resolved — and resolving a kind that was not evaluated would close an
// alert nobody had looked for.
var evaluatedKinds = []string{
	KindInstanceDown,
	KindVerificationFailed,
	KindBackupFailed,
	KindBackupMissing,
	KindRetentionBlocked,
}

// retentionBlockedDefaultHours is how long an artifact may sit expired with its object still there
// before that is worth saying out loud.
//
// Six hours rather than one: the sweep runs hourly by default and a single failure is a blip, while
// six consecutive ones is an object store that is not answering. A rule may override it through
// `threshold`.
const retentionBlockedDefaultHours = 6

// verificationFailedPredicate is the whole of ADR-0040, in one line that a test can read.
//
// It is a named constant rather than text inside a query because the change this project has to
// prevent is somebody widening it to `v.status IN ('failed', 'inconclusive')` while closing what
// looks like a gap. Named, it is one line in a diff and one failing test;
// buried in a query, it is a two-word edit nobody notices.
//
// **`failed` means the artifact was restored and the data did not match. `inconclusive` means we
// could not tell.** Alerting on the second as though it were the first is what ADR-0022 exists to
// prevent: the alert that means a backup will not restore would arrive beside a hundred that mean
// Docker was out of disk, and would be muted along with them.
const verificationFailedPredicate = "v.status = 'failed'"

// verdictFires reports whether a verification verdict produces a verification_failed alert.
//
// The intent half of ADR-0040's enforcement, beside the SQL half above. Both are asserted, because
// the two can be changed independently and only one of them is obviously about alerting.
func verdictFires(verdict string) bool { return verdict == "failed" }

// condition is one thing that is currently true, found by an evaluator.
//
// The evaluator's job ends here. What severity it carries, whether any rule asked about it, and
// whether it is new are all decided afterwards, in one place, so that a new evaluator cannot get
// any of that subtly wrong.
type condition struct {
	kind        string
	fingerprint string
	// Empty for an estate-wide condition, which only a tenant-wide rule can match.
	instanceID    string
	instanceName  string
	environmentID string
	summary       string
	detail        string
}

// EvaluationResult is what one pass did, for the log line that is its only per-pass record. There
// is no job row behind a pass (ADR-0038), so this and the rows themselves are the whole account.
type EvaluationResult struct {
	// RulesConsidered is how many enabled rules the pass read.
	RulesConsidered int
	// Firing is how many conditions were true, whether or not they were new.
	Firing int
	// Opened is how many alerts this pass created — and therefore how many notifications it sent.
	// On two control planes evaluating at once, one of them sees zero.
	Opened int
	// Resolved is how many open alerts had their condition disappear.
	Resolved int
}

// Empty reports whether the pass did nothing worth a line, which is the ordinary case on a healthy
// estate.
func (r EvaluationResult) Empty() bool {
	return r.Opened == 0 && r.Resolved == 0
}

// Evaluate runs one pass over the estate.
//
// It holds no lease, writes no job row and claims nothing, because the work is estate-wide and the
// fingerprint index is the state transition's own guard: two control planes evaluating in the same
// second produce one alert, not a race to lose (ADR-0038, mirroring ADR-0030's reasoning for the
// retention sweep).
//
// The tenant comes from the principal on the context and from nowhere else. A pass carrying no
// principal fails its first query rather than quietly reading a default tenant, which is exactly the
// failure ADR-0036 exists to make impossible.
func (s *Service) Evaluate(ctx context.Context) (EvaluationResult, error) {
	var result EvaluationResult

	tenant := authn.Tenant(ctx)
	if tenant == "" {
		return result, errors.New("alerts: evaluation ran with no principal on the context")
	}

	rules, err := s.loadEnabledRules(ctx, tenant)
	if err != nil {
		return result, err
	}
	result.RulesConsidered = len(rules)

	// A kind nobody wrote a rule for is not evaluated, and therefore not resolved either. That
	// keeps a pass on an estate with three rules from running five queries.
	wanted := map[string]bool{}
	for _, r := range rules {
		wanted[r.kind] = true
	}

	// covered is what this pass may resolve, and it is not the same list as what it evaluated.
	//
	// Three cases, and the difference between them is the whole correctness of resolution:
	//
	//   - a kind with enabled rules whose evaluator ran: covered, so a condition that has
	//     disappeared closes its alert;
	//   - a kind with enabled rules whose evaluator *failed*: **not** covered, because closing its
	//     alerts on the strength of a query that timed out would report an outage as fixed;
	//   - a kind with no enabled rule at all: covered, with no conditions. That is what makes
	//     disabling a rule resolve what it opened — "stop telling me" has to mean the row goes
	//     quiet, not that it freezes where it was.
	var (
		conditions []condition
		covered    []string
	)
	for _, kind := range evaluatedKinds {
		if !wanted[kind] {
			covered = append(covered, kind)
			continue
		}
		found, err := s.evaluateKind(ctx, kind)
		if err != nil {
			// One evaluator failing must not take the pass down with it. An estate where the
			// adherence query is slow should still be told its object store is refusing deletes.
			s.log.ErrorContext(ctx, "an alert evaluator failed; the rest of the pass continues",
				slog.String("kind", kind), slog.String("error", err.Error()),
				slog.String("consequence", "alerts of this kind are neither opened nor resolved "+
					"this pass, which is deliberate: closing one because a query failed would "+
					"report an outage as fixed"))
			continue
		}
		covered = append(covered, kind)
		conditions = append(conditions, found...)
	}
	result.Firing = len(conditions)

	live := make([]string, 0, len(conditions))
	for _, c := range conditions {
		rule, severity, ok := matchRule(rules, c)
		if !ok {
			// No enabled rule covers this instance. The condition is real and nobody asked to hear
			// about it, which is a legitimate configuration.
			continue
		}
		live = append(live, c.fingerprint)

		opened, alertID, err := s.upsertAlert(ctx, tenant, rule, severity, c)
		if err != nil {
			s.log.ErrorContext(ctx, "could not record an alert",
				slog.String("fingerprint", c.fingerprint), slog.String("error", err.Error()))
			continue
		}
		if !opened {
			continue
		}
		result.Opened++
		s.dispatch.Enqueue(ctx, Message{
			TenantID:     tenant,
			AlertID:      alertID,
			Kind:         c.kind,
			Severity:     severity,
			State:        "firing",
			InstanceID:   c.instanceID,
			InstanceName: c.instanceName,
			Summary:      c.summary,
			Detail:       c.detail,
			Fingerprint:  c.fingerprint,
			FiredAt:      time.Now().UTC(),
		})
	}

	resolved, err := s.resolveDisappeared(ctx, tenant, covered, live)
	if err != nil {
		return result, err
	}
	result.Resolved = len(resolved)
	for _, m := range resolved {
		s.dispatch.Enqueue(ctx, m)
	}
	return result, nil
}

// -----------------------------------------------------------------------------------------------
// Rules, and which one covers a condition
// -----------------------------------------------------------------------------------------------

type ruleRow struct {
	id            string
	kind          string
	severity      string
	threshold     float64
	environmentID string
	instanceID    string
}

func (s *Service) loadEnabledRules(ctx context.Context, tenant string) ([]ruleRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, kind, severity, COALESCE(threshold, 0),
		       COALESCE(environment_id::text, ''), COALESCE(instance_id::text, '')
		FROM   alert_rules
		WHERE  tenant_id = $1 AND is_enabled`, tenant)
	if err != nil {
		return nil, fmt.Errorf("alerts: load enabled rules: %w", err)
	}
	defer rows.Close()

	var out []ruleRow
	for rows.Next() {
		var r ruleRow
		if err := rows.Scan(&r.id, &r.kind, &r.severity, &r.threshold,
			&r.environmentID, &r.instanceID); err != nil {
			return nil, fmt.Errorf("alerts: read an enabled rule: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchRule picks the rule that decides a condition's severity.
//
// **The highest severity wins, and the rule id is not part of the fingerprint.** Two overlapping
// rules — one tenant-wide, one scoped to an instance — describe one broken thing, and an operator
// woken twice for one broken thing stops trusting the pager. So a condition produces at most one
// alert, whichever rule is loudest about it supplies the severity, and `alerts.rule_id` records
// which one that was.
//
// "Most specific wins" was the alternative and is wrong for the same reason it is wrong for grants
// (ADR-0034): it would turn a narrow low-severity rule into a way of *downgrading* a tenant-wide
// critical one, which is a muting mechanism nobody declared and the schema cannot express.
func matchRule(rules []ruleRow, c condition) (ruleRow, string, bool) {
	var (
		best  ruleRow
		found bool
	)
	for _, r := range rules {
		if r.kind != c.kind {
			continue
		}
		switch {
		case r.instanceID != "":
			if r.instanceID != c.instanceID {
				continue
			}
		case r.environmentID != "":
			if r.environmentID != c.environmentID {
				continue
			}
		}
		if !found || severityRankOf(r.severity) > severityRankOf(best.severity) {
			best, found = r, true
		}
	}
	if !found {
		return ruleRow{}, "", false
	}
	return best, best.severity, true
}

// thresholdFor reads a rule's threshold, falling back to the evaluator's default.
func thresholdFor(rules []ruleRow, kind string, fallback float64) float64 {
	for _, r := range rules {
		if r.kind == kind && r.threshold > 0 {
			return r.threshold
		}
	}
	return fallback
}

// -----------------------------------------------------------------------------------------------
// The upsert, and the resolve
// -----------------------------------------------------------------------------------------------

// upsertAlert records a condition, and reports whether this call is the one that opened it.
//
// Two things about the statement are load-bearing and neither is obvious.
//
// **The conflict target repeats the index's predicate.** `idx_alerts_active_fingerprint` is a
// *partial* unique index — `WHERE state <> 'resolved'` — and PostgreSQL refuses `ON CONFLICT
// (tenant_id, fingerprint)` against it with "there is no unique or exclusion constraint matching the
// ON CONFLICT specification" unless the predicate is repeated here.
//
// **`xmax = 0` is how an insert is told from an update.** RETURNING cannot otherwise distinguish
// them, and `xmax` is a system column that survives DO UPDATE. It is what makes exactly one of two
// control planes deliver a notification, and getting it wrong means every replica pages everybody
// for every alert — which is the failure people uninstall over.
//
// The DO UPDATE branch deliberately does not touch `state`. `state <> 'resolved'` covers
// `acknowledged`, so a condition that keeps being true updates an acknowledged alert and leaves it
// acknowledged: acknowledging silences a condition until it clears, and only evaluation clears it.
func (s *Service) upsertAlert(ctx context.Context, tenant string, rule ruleRow, severity string, c condition) (bool, string, error) {
	// The kind is stamped into labels rather than only implied by rule_id, because `alerts.rule_id`
	// is ON DELETE SET NULL: an alert must still be able to say what it is about after the rule that
	// found it has been deleted.
	labels := map[string]string{"kind": c.kind}
	if c.instanceID != "" {
		labels["instance_id"] = c.instanceID
	}

	var (
		instanceID *string
		alertID    string
		inserted   bool
	)
	if c.instanceID != "" {
		instanceID = &c.instanceID
	}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO alerts (tenant_id, rule_id, instance_id, severity, summary, detail, labels,
		                    fingerprint)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, fingerprint) WHERE state <> 'resolved'
		DO UPDATE SET last_seen_at = now(),
		              severity     = EXCLUDED.severity,
		              summary      = EXCLUDED.summary,
		              detail       = EXCLUDED.detail,
		              rule_id      = EXCLUDED.rule_id
		RETURNING id::text, (xmax = 0)`,
		tenant, rule.id, instanceID, severity, c.summary, c.detail, labels, c.fingerprint,
	).Scan(&alertID, &inserted)
	if err != nil {
		return false, "", fmt.Errorf("alerts: record %s: %w", c.fingerprint, err)
	}
	return inserted, alertID, nil
}

// resolveDisappeared closes alerts whose condition is no longer true, and returns what to say about
// each.
//
// Resolution is by absence, which is why it is scoped to the kinds this pass actually evaluated: an
// evaluator that failed contributes no fingerprints, and closing its alerts because of that would
// report an outage as fixed on the strength of a query timing out.
func (s *Service) resolveDisappeared(ctx context.Context, tenant string, covered, live []string) ([]Message, error) {
	if len(covered) == 0 {
		return nil, nil
	}
	if live == nil {
		live = []string{}
	}

	rows, err := s.pool.Query(ctx, `
		UPDATE alerts AS a
		SET    state = 'resolved', resolved_at = now()
		FROM   (SELECT id FROM alerts
		        WHERE  tenant_id = $1
		          AND  state <> 'resolved'
		          AND  labels->>'kind' = ANY($2::text[])
		          AND  NOT (fingerprint = ANY($3::text[]))
		        FOR UPDATE SKIP LOCKED) AS target
		WHERE  a.id = target.id
		RETURNING a.id::text, COALESCE(a.labels->>'kind', ''), a.severity,
		          COALESCE(a.instance_id::text, ''), a.summary, a.detail, a.fingerprint`,
		tenant, covered, live)
	if err != nil {
		return nil, fmt.Errorf("alerts: resolve: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		m := Message{TenantID: tenant, State: "resolved", FiredAt: time.Now().UTC()}
		if err := rows.Scan(&m.AlertID, &m.Kind, &m.Severity, &m.InstanceID,
			&m.Summary, &m.Detail, &m.Fingerprint); err != nil {
			return nil, fmt.Errorf("alerts: read a resolved alert: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// -----------------------------------------------------------------------------------------------
// The evaluators
// -----------------------------------------------------------------------------------------------

// evaluatorFor returns the function that answers one rule kind, or nil if there is none.
//
// A lookup rather than a switch inlined into the caller, so that TestEveryEvaluatedKindHasAnEvaluator
// can assert that this and `evaluatedKinds` cannot drift apart. A kind in that list with no branch
// here would be evaluated as nothing and resolved as everything — every alert of that kind closing
// itself on the next pass, silently.
func (s *Service) evaluatorFor(kind string) func(context.Context) ([]condition, error) {
	switch kind {
	case KindVerificationFailed:
		return s.verificationFailed
	case KindInstanceDown:
		return s.instanceDown
	case KindBackupFailed:
		return s.backupFailed
	case KindBackupMissing:
		return s.backupMissing
	case KindRetentionBlocked:
		return s.retentionBlocked
	default:
		return nil
	}
}

func (s *Service) evaluateKind(ctx context.Context, kind string) ([]condition, error) {
	evaluator := s.evaluatorFor(kind)
	if evaluator == nil {
		return nil, fmt.Errorf("alerts: no evaluator for rule kind %q", kind)
	}
	return evaluator(ctx)
}

// verificationFailed finds backups that were restored into a sandbox and proved unusable.
//
// **The predicate is `status = 'failed'` and it must stay that way.** An `inconclusive` verdict is
// not evidence about the artifact — a sandbox that never became ready, a plugin that could not be
// reached, a transfer that broke — and reporting it as data loss trains operators to ignore the one
// alert that means their backup will not restore. That is ADR-0022's entire purpose, and widening
// this predicate by two words would undo it (ADR-0040).
//
// TestInconclusiveVerificationDoesNotFireTheFailedAlert is what enforces it. A comment is advice; a
// test is a refusal.
func (s *Service) verificationFailed(ctx context.Context) ([]condition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.backup_id::text, b.instance_id::text, i.name, i.environment_id::text,
		       COALESCE(v.error_message, ''), v.completed_at
		FROM   verifications AS v
		JOIN   backups   AS b ON b.id = v.backup_id
		JOIN   instances AS i ON i.id = b.instance_id
		WHERE  v.tenant_id = $1
		  AND  `+verificationFailedPredicate+`
		  AND  v.id = (SELECT id FROM verifications
		               WHERE backup_id = v.backup_id AND status IN ('verified', 'failed', 'inconclusive')
		               ORDER BY created_at DESC LIMIT 1)`, authn.Tenant(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: evaluate verification_failed: %w", err)
	}
	defer rows.Close()

	var out []condition
	for rows.Next() {
		var (
			backupID, instanceID, name, environmentID, message string
			completedAt                                        *time.Time
		)
		if err := rows.Scan(&backupID, &instanceID, &name, &environmentID, &message, &completedAt); err != nil {
			return nil, fmt.Errorf("alerts: read a failed verification: %w", err)
		}
		detail := fmt.Sprintf(
			"Backup %s on %s was restored into a sandbox and the restored data did not match its "+
				"manifest. This backup should not be relied on.", backupID, name)
		if message != "" {
			detail += " The verification reported: " + message
		}
		out = append(out, condition{
			kind:          KindVerificationFailed,
			fingerprint:   KindVerificationFailed + ":" + backupID,
			instanceID:    instanceID,
			instanceName:  name,
			environmentID: environmentID,
			summary:       "A backup of " + name + " failed verification",
			detail:        detail,
		})
	}
	return out, rows.Err()
}

// instanceDown finds servers that stopped answering.
//
// `instances.health` is written by the `discovery` job, which probes through the same TestConnection
// a human runs. An instance that is *down* is a successful probe, which is why this reads a column
// rather than a job's outcome.
func (s *Service) instanceDown(ctx context.Context) ([]condition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, environment_id::text, COALESCE(health_message, ''), last_seen_at
		FROM   instances
		WHERE  tenant_id = $1 AND is_active AND health = 'HEALTH_STATE_DOWN'`, authn.Tenant(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: evaluate instance_down: %w", err)
	}
	defer rows.Close()

	var out []condition
	for rows.Next() {
		var (
			id, name, environmentID, message string
			lastSeen                         *time.Time
		)
		if err := rows.Scan(&id, &name, &environmentID, &message, &lastSeen); err != nil {
			return nil, fmt.Errorf("alerts: read an unhealthy instance: %w", err)
		}
		detail := name + " did not answer its last health probe."
		if message != "" {
			detail += " " + message
		}
		if lastSeen != nil {
			detail += fmt.Sprintf(" It was last reachable %s.", lastSeen.UTC().Format(time.RFC3339))
		}
		out = append(out, condition{
			kind:          KindInstanceDown,
			fingerprint:   KindInstanceDown + ":" + id,
			instanceID:    id,
			instanceName:  name,
			environmentID: environmentID,
			summary:       name + " is not answering",
			detail:        detail,
		})
	}
	return out, rows.Err()
}

// backupFailed finds instances whose most recent backup attempt failed.
//
// Deliberately not the same question as backupMissing. This one needs no declaration, so it fires on
// an instance nobody has written an expectation for yet — which, on the day Fleetward is installed,
// is every instance.
func (s *Service) backupFailed(ctx context.Context) ([]condition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (b.instance_id)
		       b.id::text, b.instance_id::text, i.name, i.environment_id::text,
		       COALESCE(b.error_message, ''), b.started_at, b.state
		FROM   backups   AS b
		JOIN   instances AS i ON i.id = b.instance_id
		WHERE  b.tenant_id = $1 AND i.is_active
		  AND  b.state IN ('succeeded', 'failed')
		ORDER  BY b.instance_id, b.started_at DESC`, authn.Tenant(ctx))
	if err != nil {
		return nil, fmt.Errorf("alerts: evaluate backup_failed: %w", err)
	}
	defer rows.Close()

	var out []condition
	for rows.Next() {
		var (
			backupID, instanceID, name, environmentID, message, state string
			startedAt                                                 *time.Time
		)
		if err := rows.Scan(&backupID, &instanceID, &name, &environmentID,
			&message, &startedAt, &state); err != nil {
			return nil, fmt.Errorf("alerts: read a backup outcome: %w", err)
		}
		if state != "failed" {
			continue
		}
		detail := "The most recent backup attempt on " + name + " failed."
		if message != "" {
			detail += " It reported: " + message
		}
		if startedAt != nil {
			detail += fmt.Sprintf(" The attempt started %s.", startedAt.UTC().Format(time.RFC3339))
		}
		out = append(out, condition{
			kind:          KindBackupFailed,
			fingerprint:   KindBackupFailed + ":" + instanceID,
			instanceID:    instanceID,
			instanceName:  name,
			environmentID: environmentID,
			summary:       "The last backup of " + name + " failed",
			detail:        detail,
		})
	}
	return out, rows.Err()
}

// backupMissing finds windows that closed empty.
//
// It calls the adherence computation rather than reimplementing it, which is the point: adherence is
// computed on read and stores no verdict, so an alert and the estate view can never disagree because
// they are the same function. `adherence.go` says so in as many words, and this is the caller it was
// anticipating.
//
// **`NOT_DECLARED` is not a problem.** An instance nobody has written an expectation for produces no
// condition here. Firing on it would mean an alert per instance on the first pass of a fresh
// installation, which is how alerting gets switched off on day one.
func (s *Service) backupMissing(ctx context.Context) ([]condition, error) {
	if s.adherence == nil {
		return nil, errors.New("alerts: no adherence source is wired")
	}
	answers, err := s.adherence.InstanceAdherence(ctx)
	if err != nil {
		return nil, fmt.Errorf("alerts: evaluate backup_missing: %w", err)
	}

	var out []condition
	for _, a := range answers {
		if a.GetState() != fwv1.AdherenceState_ADHERENCE_STATE_MISSED {
			continue
		}
		name := a.GetInstanceName()
		detail := fmt.Sprintf("A backup of %s was expected by %s and none arrived.",
			name, a.GetDeadline().AsTime().UTC().Format(time.RFC3339))
		if latest := a.GetLatestBackup(); latest != nil && latest.GetCompletedAt() != nil {
			detail += " The most recent backup of any kind was " +
				latest.GetCompletedAt().AsTime().UTC().Format(time.RFC3339) + "."
		} else {
			detail += " There is no record of any backup of this instance."
		}
		if caveats := a.GetCaveats(); len(caveats) > 0 {
			detail += " What weakens this answer: " + strings.Join(caveats, "; ") + "."
		}
		out = append(out, condition{
			kind:          KindBackupMissing,
			fingerprint:   KindBackupMissing + ":" + a.GetInstanceId(),
			instanceID:    a.GetInstanceId(),
			instanceName:  name,
			environmentID: a.GetEnvironmentId(),
			summary:       "A backup window closed with nothing in it on " + name,
			detail:        detail,
		})
	}
	return out, nil
}

// retentionBlocked finds artifacts the object store has refused to delete.
//
// The sweep already leaves its backlog in rows: `expireOutlivedBackups` commits the
// `succeeded -> expired` transition on its own, and `deleteExpiredArtifacts` clears `object_key`
// only once the object is actually gone. So a row that is expired with a key still on it is an
// object the store would not remove, and the oldest such row's age is how long that has been going
// on. `RetentionResult.Unreachable` counts the same thing for one log line and persists nothing,
// which is precisely why this reads rows instead.
//
// One alert for the estate rather than one per artifact: the fault is the object store, not the
// fifty instances whose artifacts are queued behind it.
func (s *Service) retentionBlocked(ctx context.Context) ([]condition, error) {
	rules, err := s.loadEnabledRules(ctx, authn.Tenant(ctx))
	if err != nil {
		return nil, err
	}
	hours := thresholdFor(rules, KindRetentionBlocked, retentionBlockedDefaultHours)

	var (
		stuck  int
		oldest *time.Time
		bytes  int64
	)
	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*), MIN(updated_at), COALESCE(SUM(size_bytes), 0)
		FROM   backups
		WHERE  tenant_id = $1 AND state = 'expired' AND object_key <> ''
		  AND  updated_at < now() - make_interval(hours => $2::int)`,
		authn.Tenant(ctx), int(hours)).Scan(&stuck, &oldest, &bytes)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("alerts: evaluate retention_blocked: %w", err)
	}
	if stuck == 0 || oldest == nil {
		return nil, nil
	}

	return []condition{{
		kind:        KindRetentionBlocked,
		fingerprint: KindRetentionBlocked + ":estate",
		summary:     fmt.Sprintf("%d expired backup artifacts could not be deleted", stuck),
		detail: fmt.Sprintf(
			"The retention sweep has been unable to remove %d artifacts totalling %d bytes. The "+
				"oldest has been queued since %s, which is longer than the %g hours this rule "+
				"allows. Nothing has been lost — the rows are intact and each sweep tries again — "+
				"but storage is not being reclaimed, so the object store is worth looking at.",
			stuck, bytes, oldest.UTC().Format(time.RFC3339), hours),
	}}, nil
}

// -----------------------------------------------------------------------------------------------
// Vocabulary
// -----------------------------------------------------------------------------------------------

// hasEvaluator reports whether a rule of this kind would ever fire.
func hasEvaluator(kind fwv1.AlertKind) bool {
	name, err := ruleKindString(kind)
	if err != nil {
		return false
	}
	for _, k := range evaluatedKinds {
		if k == name {
			return true
		}
	}
	return false
}

func ruleKindString(kind fwv1.AlertKind) (string, error) {
	switch kind {
	case fwv1.AlertKind_ALERT_KIND_INSTANCE_DOWN:
		return KindInstanceDown, nil
	case fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED:
		return KindVerificationFailed, nil
	case fwv1.AlertKind_ALERT_KIND_BACKUP_FAILED:
		return KindBackupFailed, nil
	case fwv1.AlertKind_ALERT_KIND_BACKUP_MISSING:
		return KindBackupMissing, nil
	case fwv1.AlertKind_ALERT_KIND_STORAGE_THRESHOLD:
		return KindStorageThreshold, nil
	case fwv1.AlertKind_ALERT_KIND_REPLICATION_LAG:
		return KindReplicationLag, nil
	case fwv1.AlertKind_ALERT_KIND_CUSTOM_PROMQL:
		return KindCustomPromQL, nil
	case fwv1.AlertKind_ALERT_KIND_RETENTION_BLOCKED:
		return KindRetentionBlocked, nil
	case fwv1.AlertKind_ALERT_KIND_UNSPECIFIED:
		return "", fmt.Errorf("%w: kind is required", ErrInvalidArgument)
	default:
		return "", fmt.Errorf("%w: unknown alert kind %d", ErrInvalidArgument, kind)
	}
}

func ruleKindEnum(kind string) fwv1.AlertKind {
	switch kind {
	case KindInstanceDown:
		return fwv1.AlertKind_ALERT_KIND_INSTANCE_DOWN
	case KindVerificationFailed:
		return fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED
	case KindBackupFailed:
		return fwv1.AlertKind_ALERT_KIND_BACKUP_FAILED
	case KindBackupMissing:
		return fwv1.AlertKind_ALERT_KIND_BACKUP_MISSING
	case KindStorageThreshold:
		return fwv1.AlertKind_ALERT_KIND_STORAGE_THRESHOLD
	case KindReplicationLag:
		return fwv1.AlertKind_ALERT_KIND_REPLICATION_LAG
	case KindCustomPromQL:
		return fwv1.AlertKind_ALERT_KIND_CUSTOM_PROMQL
	case KindRetentionBlocked:
		return fwv1.AlertKind_ALERT_KIND_RETENTION_BLOCKED
	default:
		return fwv1.AlertKind_ALERT_KIND_UNSPECIFIED
	}
}

func severityString(s fwv1.AlertSeverity) string {
	switch s {
	case fwv1.AlertSeverity_ALERT_SEVERITY_INFO:
		return "info"
	case fwv1.AlertSeverity_ALERT_SEVERITY_WARNING:
		return "warning"
	case fwv1.AlertSeverity_ALERT_SEVERITY_CRITICAL:
		return "critical"
	case fwv1.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED:
		return ""
	default:
		return ""
	}
}

func severityEnum(s string) fwv1.AlertSeverity {
	switch s {
	case "info":
		return fwv1.AlertSeverity_ALERT_SEVERITY_INFO
	case "warning":
		return fwv1.AlertSeverity_ALERT_SEVERITY_WARNING
	case "critical":
		return fwv1.AlertSeverity_ALERT_SEVERITY_CRITICAL
	default:
		return fwv1.AlertSeverity_ALERT_SEVERITY_UNSPECIFIED
	}
}

func alertStateEnum(s string) fwv1.AlertState {
	switch s {
	case "firing":
		return fwv1.AlertState_ALERT_STATE_FIRING
	case "acknowledged":
		return fwv1.AlertState_ALERT_STATE_ACKNOWLEDGED
	case "resolved":
		return fwv1.AlertState_ALERT_STATE_RESOLVED
	default:
		return fwv1.AlertState_ALERT_STATE_UNSPECIFIED
	}
}

// defaultSeverityFor is what a rule gets when its creator did not say.
//
// A failed verification is critical and the other four are warnings, which is the product's own
// judgement about which of these means "your backup will not restore".
func defaultSeverityFor(kind fwv1.AlertKind) string {
	if kind == fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED {
		return "critical"
	}
	return "warning"
}

func severityRankOf(s string) int {
	switch s {
	case "info":
		return 1
	case "warning":
		return 2
	case "critical":
		return 3
	default:
		return 0
	}
}

func severityRank(s fwv1.AlertSeverity) int { return severityRankOf(severityString(s)) }

// severityRankSQL orders and compares severities in the database, where they are text.
//
// A CASE expression rather than an enum type or a rank column: the CHECK constraint is the schema's
// statement about what a severity may be, and adding a lookup table to sort three values would be
// more machinery than the problem has.
func severityRankSQL(column string) string {
	return "(CASE " + column + " WHEN 'critical' THEN 3 WHEN 'warning' THEN 2 WHEN 'info' THEN 1 ELSE 0 END)"
}
