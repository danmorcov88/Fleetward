// Package alerts turns conditions Fleetward can already detect into rows somebody is told about.
//
// Six slices built a product that knows things and tells nobody. It knows a verification failed,
// that a backup window closed empty, that an instance stopped answering, that the object store has
// refused every delete for a week — and every one of those facts is reachable only by somebody
// deciding to look. This package is the difference between a dashboard and monitoring.
//
// Four shapes are worth knowing before reading further.
//
// **Nothing here detects anything new.** Every evaluator reads a computation that already answers a
// question the API answers: `GetBackupAdherence` for a missed window, `verifications.status` for a
// failed verification, `instances.health` for a silent server, and retention's own backlog of
// `expired` rows whose objects are still there. So an alert and the estate view can never disagree,
// because they are the same function (ADR-0038).
//
// **A rule kind selects an evaluator and never an engine.** `alert_rules.kind` is a closed set with
// a CHECK constraint, widened only by migration. Nothing in this package reads
// `instances.engine_type`, and a search for an engine name in here should find nothing
// (CLAUDE.md §4.1).
//
// **FAILED and INCONCLUSIVE are different answers and must not produce the same alert.** ADR-0022
// exists so that "your backup will not restore" is never muted by a flood of "Docker was out of
// disk". `verification_failed` fires on a verdict of `failed` and on nothing else, and
// TestInconclusiveVerificationDoesNotFireTheFailedAlert is what keeps it that way (ADR-0040).
//
// **The alert row is the record; the notification is best-effort.** Delivery is at-most-once with
// no durable outbox. The row survives a webhook that was down, and `notifiers.last_error` is what
// makes a destination that has been failing all week visible rather than silent (ADR-0039).
package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
)

// Sentinel errors, classified with errors.Is by the gRPC layer, which is the only place that
// decides what a client sees.
var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidArgument = errors.New("invalid argument")
)

const (
	defaultAlertPageSize = 100
	maxAlertPageSize     = 500

	// secretNamePrefix keeps a notifier's credential in its own namespace within the tenant's
	// secrets, beside `connection/<uuid>` which inventory owns.
	secretNamePrefix = "notifier/"
)

// Adherence is the slice of the backup service this package needs.
//
// An interface rather than the concrete service for one reason that matters: the evaluator's tests
// exercise what a MISSED window turns into, and none of that should require an object store, a
// plugin process or a container runtime to be present.
type Adherence interface {
	// InstanceAdherence answers "did every server's backup run when it was supposed to" for the
	// caller on ctx. The evaluator calls it as a system principal, which sees the whole estate.
	InstanceAdherence(ctx context.Context) ([]*fwv1.InstanceAdherence, error)
}

// Service owns alert rules, alerts, and the destinations alerts are delivered to.
type Service struct {
	pool      *pgxpool.Pool
	secrets   secrets.Provider
	adherence Adherence
	dispatch  *Dispatcher
	log       *slog.Logger
}

// New builds the service.
func New(pool *pgxpool.Pool, secretsProvider secrets.Provider, adherence Adherence, dispatch *Dispatcher, log *slog.Logger) *Service {
	return &Service{
		pool:      pool,
		secrets:   secretsProvider,
		adherence: adherence,
		dispatch:  dispatch,
		log:       log.With(slog.String("component", "alerts")),
	}
}

// -----------------------------------------------------------------------------------------------
// Rules
// -----------------------------------------------------------------------------------------------

// ListAlertRules reports the rules declared in this tenant.
func (s *Service) ListAlertRules(ctx context.Context, includeDisabled bool) ([]*fwv1.AlertRule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, description, kind, severity, COALESCE(threshold, 0),
		       COALESCE(environment_id::text, ''), COALESCE(instance_id::text, ''),
		       is_enabled, created_at, updated_at
		FROM   alert_rules
		WHERE  tenant_id = $1 AND ($2::boolean OR is_enabled)
		ORDER  BY name`, authn.Tenant(ctx), includeDisabled)
	if err != nil {
		return nil, fmt.Errorf("alerts: list rules: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.AlertRule
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// CreateRuleInput is one declared condition.
type CreateRuleInput struct {
	Name          string
	Description   string
	Kind          fwv1.AlertKind
	Severity      fwv1.AlertSeverity
	Threshold     float64
	EnvironmentID string
	InstanceID    string
}

// CreateAlertRule declares a condition worth being told about.
//
// A kind with no evaluator is refused rather than stored. That is deliberate and it is the same
// treatment `schedules.kind = 'metrics'` already gets: a rule accepted and never evaluated is worse
// than one refused, because the operator believes they are covered and finds out otherwise during
// the incident it was supposed to catch.
func (s *Service) CreateAlertRule(ctx context.Context, in CreateRuleInput) (*fwv1.AlertRule, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}
	kind, err := ruleKindString(in.Kind)
	if err != nil {
		return nil, err
	}
	if !hasEvaluator(in.Kind) {
		return nil, fmt.Errorf(
			"%w: no evaluator exists for rule kind %q yet, so a rule of that kind would never fire; "+
				"the kinds that are evaluated today are instance_down, verification_failed, "+
				"backup_failed, backup_missing and retention_blocked",
			ErrInvalidArgument, kind)
	}
	severity := severityString(in.Severity)
	if severity == "" {
		severity = defaultSeverityFor(in.Kind)
	}
	if in.EnvironmentID != "" && in.InstanceID != "" {
		return nil, fmt.Errorf(
			"%w: a rule is scoped to an environment or to an instance, not to both", ErrInvalidArgument)
	}

	var (
		environmentID = nullableUUID(in.EnvironmentID)
		instanceID    = nullableUUID(in.InstanceID)
	)
	if in.EnvironmentID != "" && environmentID == nil {
		return nil, fmt.Errorf("%w: environment_id must be a UUID", ErrInvalidArgument)
	}
	if in.InstanceID != "" && instanceID == nil {
		return nil, fmt.Errorf("%w: instance_id must be a UUID", ErrInvalidArgument)
	}

	var threshold *float64
	if in.Threshold != 0 {
		threshold = &in.Threshold
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO alert_rules (tenant_id, name, description, kind, severity, threshold,
		                         environment_id, instance_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id::text, name, description, kind, severity, COALESCE(threshold, 0),
		          COALESCE(environment_id::text, ''), COALESCE(instance_id::text, ''),
		          is_enabled, created_at, updated_at`,
		authn.Tenant(ctx), name, in.Description, kind, severity, threshold, environmentID, instanceID)

	rule, err := scanRule(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: a rule named %q already exists", ErrInvalidArgument, name)
		}
		return nil, err
	}
	return rule, nil
}

// SetAlertRuleEnabled pauses or resumes a rule.
//
// Disabling is the whole silencing story in this version, and it is honest about being that: the
// next evaluation pass sees no enabled rule of that kind covering those instances, and resolves the
// alerts it had opened. "Stop telling me" has to mean the row goes quiet rather than that it freezes
// where it was.
func (s *Service) SetAlertRuleEnabled(ctx context.Context, ruleID string, enabled bool) (*fwv1.AlertRule, error) {
	id := nullableUUID(ruleID)
	if id == nil {
		return nil, fmt.Errorf("%w: rule_id must be a UUID", ErrInvalidArgument)
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE alert_rules
		SET    is_enabled = $3, updated_at = now()
		WHERE  id = $1 AND tenant_id = $2
		RETURNING id::text, name, description, kind, severity, COALESCE(threshold, 0),
		          COALESCE(environment_id::text, ''), COALESCE(instance_id::text, ''),
		          is_enabled, created_at, updated_at`,
		ruleID, authn.Tenant(ctx), enabled)

	rule, err := scanRule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no alert rule %s", ErrNotFound, ruleID)
	}
	return rule, err
}

// DeleteAlertRule removes a rule and resolves the alerts it opened, in one transaction.
//
// The second half is not tidiness. `alerts.rule_id` is ON DELETE SET NULL, so deleting a rule on its
// own would leave its alerts behind with nothing pointing at them — and evaluation resolves an alert
// by not finding its condition among the *rules it evaluated*, which an orphan is no longer part of.
// They would stay firing forever, and nothing in the product would explain why.
func (s *Service) DeleteAlertRule(ctx context.Context, ruleID string) (int32, error) {
	if nullableUUID(ruleID) == nil {
		return 0, fmt.Errorf("%w: rule_id must be a UUID", ErrInvalidArgument)
	}
	tenant := authn.Tenant(ctx)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("alerts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE alerts
		SET    state = 'resolved', resolved_at = now()
		WHERE  tenant_id = $1 AND rule_id = $2 AND state <> 'resolved'`, tenant, ruleID)
	if err != nil {
		return 0, fmt.Errorf("alerts: resolve a deleted rule's alerts: %w", err)
	}
	resolved := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1 AND tenant_id = $2`, ruleID, tenant)
	if err != nil {
		return 0, fmt.Errorf("alerts: delete rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return 0, fmt.Errorf("%w: no alert rule %s", ErrNotFound, ruleID)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("alerts: commit: %w", err)
	}
	return int32(resolved), nil //nolint:gosec // G115: a tenant's open alerts do not reach 2^31
}

// -----------------------------------------------------------------------------------------------
// Alerts
// -----------------------------------------------------------------------------------------------

// ListAlertsInput filters the estate the question is asked about.
type ListAlertsInput struct {
	InstanceID      string
	EnvironmentID   string
	IncludeResolved bool
	MinSeverity     fwv1.AlertSeverity
	PageSize        int32
}

// ListAlerts reports what is wrong, or was.
//
// It filters its own rows by the caller's grants, which is why its policy entry carries
// ScopeFiltered. Without that a request naming no instance is a question about the estate, and a
// person granted `dba` on three servers would get a 403 for asking what is wrong with them
// (ADR-0035). The flag says the method filters; this query is the filtering.
func (s *Service) ListAlerts(ctx context.Context, in ListAlertsInput) ([]*fwv1.Alert, error) {
	visible := authn.VisibilityFor(ctx, 0)
	if visible.Empty() {
		return nil, nil
	}

	pageSize := in.PageSize
	if pageSize <= 0 {
		pageSize = defaultAlertPageSize
	}
	if pageSize > maxAlertPageSize {
		pageSize = maxAlertPageSize
	}

	// An estate-wide alert — retention_blocked — has no instance, so it cannot be covered by an
	// instance-scoped grant and is shown only to a caller who can see the whole tenant. That is the
	// same rule the rest of the product applies to a question that names no scope.
	args := []any{authn.Tenant(ctx), visible.All, visible.EnvironmentIDs, visible.InstanceIDs}
	filters := ` AND ($2::boolean
	                  OR i.environment_id = ANY($3::uuid[])
	                  OR a.instance_id = ANY($4::uuid[]))`
	if !in.IncludeResolved {
		filters += ` AND a.state <> 'resolved'`
	}
	if in.InstanceID != "" {
		if nullableUUID(in.InstanceID) == nil {
			return nil, fmt.Errorf("%w: instance_id must be a UUID", ErrInvalidArgument)
		}
		args = append(args, in.InstanceID)
		filters += fmt.Sprintf(" AND a.instance_id = $%d", len(args))
	}
	if in.EnvironmentID != "" {
		if nullableUUID(in.EnvironmentID) == nil {
			return nil, fmt.Errorf("%w: environment_id must be a UUID", ErrInvalidArgument)
		}
		args = append(args, in.EnvironmentID)
		filters += fmt.Sprintf(" AND i.environment_id = $%d", len(args))
	}
	if rank := severityRank(in.MinSeverity); rank > 0 {
		args = append(args, rank)
		filters += fmt.Sprintf(" AND %s >= $%d", severityRankSQL("a.severity"), len(args))
	}
	args = append(args, pageSize)

	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, COALESCE(a.rule_id::text, ''), COALESCE(r.name, ''),
		       COALESCE(a.labels->>'kind', ''),
		       COALESCE(a.instance_id::text, ''), COALESCE(i.name, ''),
		       a.severity, a.state, a.summary, a.detail, a.fingerprint,
		       a.started_at, a.last_seen_at, a.acknowledged_at,
		       COALESCE(u.display_name, u.subject, ''), a.resolved_at
		FROM   alerts AS a
		LEFT   JOIN alert_rules AS r ON r.id = a.rule_id
		LEFT   JOIN instances   AS i ON i.id = a.instance_id
		LEFT   JOIN users       AS u ON u.id = a.acknowledged_by
		WHERE  a.tenant_id = $1`+filters+`
		ORDER  BY `+severityRankSQL("a.severity")+` DESC, a.started_at DESC
		LIMIT  $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, fmt.Errorf("alerts: list: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.Alert
	for rows.Next() {
		alert, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, alert)
	}
	return out, rows.Err()
}

// AcknowledgeAlert records that somebody knows.
//
// It does not resolve the alert, and the distinction is the whole point: only evaluation resolves,
// because only evaluation knows whether the condition is still true. An acknowledged alert keeps
// being updated by every pass — `state <> 'resolved'` covers `acknowledged`, so the upsert finds it
// — and it stops being delivered anywhere, which is what an operator asked for.
func (s *Service) AcknowledgeAlert(ctx context.Context, alertID string) (*fwv1.Alert, error) {
	if nullableUUID(alertID) == nil {
		return nil, fmt.Errorf("%w: alert_id must be a UUID", ErrInvalidArgument)
	}
	p, err := authn.MustFrom(ctx)
	if err != nil {
		return nil, err
	}

	// NULL for a system or bootstrap caller, which have no `users` row. `audit_log.actor` carries
	// the name in that case, exactly as it does for the scheduler (ADR-0036).
	var userID *string
	if p.UserID != "" {
		userID = &p.UserID
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE alerts
		SET    state = 'acknowledged', acknowledged_at = now(), acknowledged_by = $3
		WHERE  id = $1 AND tenant_id = $2 AND state = 'firing'`,
		alertID, p.TenantID, userID)
	if err != nil {
		return nil, fmt.Errorf("alerts: acknowledge: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either it does not exist, or it is already acknowledged or resolved. Distinguishing the
		// three would mean two more queries to say something the returned row already says.
		alert, getErr := s.getAlert(ctx, alertID)
		if getErr != nil {
			return nil, getErr
		}
		return alert, nil
	}
	return s.getAlert(ctx, alertID)
}

func (s *Service) getAlert(ctx context.Context, alertID string) (*fwv1.Alert, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT a.id::text, COALESCE(a.rule_id::text, ''), COALESCE(r.name, ''),
		       COALESCE(a.labels->>'kind', ''),
		       COALESCE(a.instance_id::text, ''), COALESCE(i.name, ''),
		       a.severity, a.state, a.summary, a.detail, a.fingerprint,
		       a.started_at, a.last_seen_at, a.acknowledged_at,
		       COALESCE(u.display_name, u.subject, ''), a.resolved_at
		FROM   alerts AS a
		LEFT   JOIN alert_rules AS r ON r.id = a.rule_id
		LEFT   JOIN instances   AS i ON i.id = a.instance_id
		LEFT   JOIN users       AS u ON u.id = a.acknowledged_by
		WHERE  a.id = $1 AND a.tenant_id = $2`, alertID, authn.Tenant(ctx))

	alert, err := scanAlert(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: no alert %s", ErrNotFound, alertID)
	}
	return alert, err
}

// -----------------------------------------------------------------------------------------------
// Notifiers
// -----------------------------------------------------------------------------------------------

// ListNotifiers reports where alerts are sent, and how that has been going.
//
// The credential is never in the answer. `has_secret` says whether one is stored, which is the only
// thing a reader needs and the most a reader may have.
func (s *Service) ListNotifiers(ctx context.Context, includeDisabled bool) ([]*fwv1.Notifier, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, kind, settings, secret_name <> '', min_severity, is_enabled,
		       last_attempt_at, last_success_at, last_error, created_at
		FROM   notifiers
		WHERE  tenant_id = $1 AND ($2::boolean OR is_enabled)
		ORDER  BY name`, authn.Tenant(ctx), includeDisabled)
	if err != nil {
		return nil, fmt.Errorf("alerts: list notifiers: %w", err)
	}
	defer rows.Close()

	var out []*fwv1.Notifier
	for rows.Next() {
		n, err := scanNotifier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateNotifierInput is one destination.
type CreateNotifierInput struct {
	Name        string
	Kind        string
	Settings    map[string]string
	Secret      string
	MinSeverity fwv1.AlertSeverity
}

// CreateNotifier stores a destination and, separately, its credential.
//
// The credential goes through the SecretsProvider under `notifier/<uuid>` and never into `settings`,
// which is JSONB that every administrator listing notifiers can read. `settings` is validated for
// credential-shaped keys and the create is refused if it has any — see credentialShapedKey for why
// that check exists rather than a comment asking people not to.
func (s *Service) CreateNotifier(ctx context.Context, in CreateNotifierInput) (*fwv1.Notifier, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind != NotifierWebhook && kind != NotifierSMTP {
		return nil, fmt.Errorf("%w: notifier kind must be %q or %q, got %q",
			ErrInvalidArgument, NotifierWebhook, NotifierSMTP, in.Kind)
	}
	if bad := credentialShapedKey(in.Settings); bad != "" {
		return nil, fmt.Errorf(
			"%w: settings key %q looks like a credential, and settings is readable by every "+
				"administrator who lists notifiers; pass it as the notifier's secret instead",
			ErrInvalidArgument, bad)
	}
	if err := validateSettings(kind, in.Settings); err != nil {
		return nil, err
	}
	severity := severityString(in.MinSeverity)
	if severity == "" {
		severity = "warning"
	}

	settings := in.Settings
	if settings == nil {
		settings = map[string]string{}
	}

	tenant := authn.Tenant(ctx)

	// The row first, so that the secret has an id to hang from, then the secret, then the row is
	// pointed at it. A failure between the second and the third leaves an orphaned secret rather
	// than a notifier claiming a credential it does not have — the safe way round, since an orphan
	// is invisible and a phantom reference is a delivery that fails at 3am.
	var id string
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO notifiers (tenant_id, name, kind, settings, min_severity)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text`, tenant, name, kind, settings, severity).Scan(&id); err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: a notifier named %q already exists", ErrInvalidArgument, name)
		}
		return nil, fmt.Errorf("alerts: create notifier: %w", err)
	}

	if in.Secret != "" {
		secretName := secretNamePrefix + id
		if err := s.secrets.Put(ctx,
			secrets.Ref{TenantID: tenant, Name: secretName}, []byte(in.Secret)); err != nil {
			// Never the secret, and never the error's payload: a provider's message is the one
			// place a fragment of one could reach a log.
			return nil, fmt.Errorf("alerts: store the notifier's credential: %w", err)
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE notifiers SET secret_name = $3 WHERE id = $1 AND tenant_id = $2`,
			id, tenant, secretName); err != nil {
			return nil, fmt.Errorf("alerts: link the notifier's credential: %w", err)
		}
	}

	notifiers, err := s.ListNotifiers(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, n := range notifiers {
		if n.GetId() == id {
			return n, nil
		}
	}
	return nil, fmt.Errorf("alerts: created notifier %s could not be read back", id)
}

// DeleteNotifier removes a destination and the credential it held.
func (s *Service) DeleteNotifier(ctx context.Context, notifierID string) error {
	if nullableUUID(notifierID) == nil {
		return fmt.Errorf("%w: notifier_id must be a UUID", ErrInvalidArgument)
	}
	tenant := authn.Tenant(ctx)

	var secretName string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM notifiers WHERE id = $1 AND tenant_id = $2 RETURNING secret_name`,
		notifierID, tenant).Scan(&secretName)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no notifier %s", ErrNotFound, notifierID)
	}
	if err != nil {
		return fmt.Errorf("alerts: delete notifier: %w", err)
	}

	if secretName != "" {
		// Deleting a secret that is not there is not an error, by the provider's contract. A
		// failure here leaves a credential nothing references, which is worth a line and is not
		// worth failing a delete the operator asked for.
		if err := s.secrets.Delete(ctx, secrets.Ref{TenantID: tenant, Name: secretName}); err != nil {
			s.log.WarnContext(ctx, "deleted a notifier but could not remove its stored credential",
				slog.String("notifier_id", notifierID),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// TestNotifier sends a real message to a real endpoint.
//
// It exists because "is my webhook configured correctly" has exactly two moments it can be asked:
// now, or during the incident the notifier was configured for. Delivery is at-most-once and
// best-effort (ADR-0039), so an operator who never tests one finds out at the second moment.
func (s *Service) TestNotifier(ctx context.Context, notifierID string) (bool, string, time.Duration, error) {
	if nullableUUID(notifierID) == nil {
		return false, "", 0, fmt.Errorf("%w: notifier_id must be a UUID", ErrInvalidArgument)
	}
	// Synchronously, unlike a real notification: the caller is a person waiting for an answer, and
	// "queued" is not one.
	return s.dispatch.Test(ctx, authn.Tenant(ctx), notifierID)
}

// -----------------------------------------------------------------------------------------------
// Scanning
// -----------------------------------------------------------------------------------------------

// scannable is what both a pgx.Row and a pgx.Rows satisfy, so one scan function serves a single-row
// query and a listing.
type scannable interface {
	Scan(dest ...any) error
}

func scanRule(row scannable) (*fwv1.AlertRule, error) {
	var (
		rule                             fwv1.AlertRule
		kind, severity                   string
		createdAt, updatedAt             time.Time
		environmentID, instanceID, descr string
	)
	if err := row.Scan(&rule.Id, &rule.Name, &descr, &kind, &severity, &rule.Threshold,
		&environmentID, &instanceID, &rule.IsEnabled, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("alerts: read an alert rule: %w", err)
	}
	rule.Description = descr
	rule.Kind = ruleKindEnum(kind)
	rule.Severity = severityEnum(severity)
	rule.EnvironmentId = environmentID
	rule.InstanceId = instanceID
	rule.CreatedAt = timestamppb.New(createdAt)
	rule.UpdatedAt = timestamppb.New(updatedAt)
	return &rule, nil
}

func scanAlert(row scannable) (*fwv1.Alert, error) {
	var (
		alert                                     fwv1.Alert
		kind, severity, state                     string
		startedAt, lastSeenAt                     time.Time
		acknowledgedAt, resolvedAt                *time.Time
		ruleID, ruleName, instanceID, instanceNam string
		acknowledgedBy                            string
	)
	if err := row.Scan(&alert.Id, &ruleID, &ruleName, &kind, &instanceID, &instanceNam,
		&severity, &state, &alert.Summary, &alert.Detail, &alert.Fingerprint,
		&startedAt, &lastSeenAt, &acknowledgedAt, &acknowledgedBy, &resolvedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("alerts: read an alert: %w", err)
	}
	alert.RuleId = ruleID
	alert.RuleName = ruleName
	alert.Kind = ruleKindEnum(kind)
	alert.InstanceId = instanceID
	alert.InstanceName = instanceNam
	alert.Severity = severityEnum(severity)
	alert.State = alertStateEnum(state)
	alert.StartedAt = timestamppb.New(startedAt)
	alert.LastSeenAt = timestamppb.New(lastSeenAt)
	alert.AcknowledgedBy = acknowledgedBy
	if acknowledgedAt != nil {
		alert.AcknowledgedAt = timestamppb.New(*acknowledgedAt)
	}
	if resolvedAt != nil {
		alert.ResolvedAt = timestamppb.New(*resolvedAt)
	}
	return &alert, nil
}

func scanNotifier(row scannable) (*fwv1.Notifier, error) {
	var (
		n                        fwv1.Notifier
		settings                 map[string]string
		minSeverity              string
		lastAttempt, lastSuccess *time.Time
		createdAt                time.Time
		hasSecret, isEnabled     bool
		id, name, kind, lastErr  string
	)
	if err := row.Scan(&id, &name, &kind, &settings, &hasSecret, &minSeverity, &isEnabled,
		&lastAttempt, &lastSuccess, &lastErr, &createdAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("alerts: read a notifier: %w", err)
	}
	n.Id = id
	n.Name = name
	n.Kind = kind
	n.Settings = settings
	n.HasSecret = hasSecret
	n.MinSeverity = severityEnum(minSeverity)
	n.IsEnabled = isEnabled
	n.LastError = lastErr
	n.CreatedAt = timestamppb.New(createdAt)
	if lastAttempt != nil {
		n.LastAttemptAt = timestamppb.New(*lastAttempt)
	}
	if lastSuccess != nil {
		n.LastSuccessAt = timestamppb.New(*lastSuccess)
	}
	return &n, nil
}

// -----------------------------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------------------------

// credentialShapedKey reports the first settings key that looks like a credential.
//
// The check exists rather than a comment asking people not to, because `settings` is returned by
// ListNotifiers and the shortcut — "just put the token in settings, it is easier" — is exactly the
// one somebody takes at 2am. A Slack webhook URL that embeds its own token is the case this cannot
// catch, and `docs/ops/alerting.md` says so rather than leaving it to be discovered.
func credentialShapedKey(settings map[string]string) string {
	shapes := []string{"password", "token", "secret", "api_key", "apikey", "authorization"}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys) // a map's iteration order must not decide which error a caller sees
	for _, k := range keys {
		lower := strings.ToLower(k)
		for _, shape := range shapes {
			if strings.Contains(lower, shape) {
				return k
			}
		}
	}
	return ""
}

func nullableUUID(value string) *string {
	if value == "" || !metadb.IsUUID(value) {
		return nil
	}
	return &value
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate key value")
}
