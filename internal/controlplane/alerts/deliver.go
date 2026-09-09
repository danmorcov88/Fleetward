package alerts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/danmorcov88/fleetward/internal/storage/secrets"
)

// The two notifier kinds, spelled as the CHECK constraint spells them.
//
// A Slack incoming webhook is a webhook and a Teams connector is a webhook. A first-class kind per
// vendor buys a nicer message body and costs a migration, and it is not what stands between this
// product and being trusted.
const (
	NotifierWebhook = "webhook"
	NotifierSMTP    = "smtp"
)

// Message is what a notifier is asked to deliver.
//
// It carries everything a destination needs and nothing it should not: no credential, no connection
// string, and no row identifier a recipient could not already read through the API.
type Message struct {
	TenantID string
	AlertID  string
	Kind     string
	Severity string
	// "firing", "resolved", or "test". Delivery happens on transitions only: a rule that keeps
	// firing updates last_seen_at and notifies nobody, which is what the fingerprint is for.
	State        string
	InstanceID   string
	InstanceName string
	Summary      string
	Detail       string
	Fingerprint  string
	FiredAt      time.Time
}

// DispatchConfig bounds what delivery may consume.
//
// Every field is a limit rather than a feature, for the same reason RetentionConfig's are: this is
// the code that reaches out of the process to somebody else's system, and a slow endpoint must not
// be able to stall the pass that found the alert.
type DispatchConfig struct {
	// Workers is how many notifications are in flight at once.
	Workers int
	// QueueSize bounds what is waiting. A full queue drops rather than blocks — see Enqueue.
	QueueSize int
	// Timeout bounds one delivery attempt.
	Timeout time.Duration
	// Attempts is how many times one notification is tried before it is given up on. Bounded and
	// small: this is a retry, not an outbox (ADR-0039).
	Attempts int
}

// Dispatcher sends notifications, at most once each.
//
// **The alert row is the record and this is a convenience.** There is no `notifications` table, no
// durable outbox, no backoff schedule and no dead-letter queue. A notification that cannot be
// delivered after `Attempts` tries is logged, recorded on the notifier's row, and dropped; the alert
// remains, and the API and the CLI still show it (ADR-0039).
//
// That is a real limitation with a real cost, and it is stated where an operator will read it:
// `docs/ops/alerting.md` says outright that the absence of an email is not evidence that nothing is
// wrong. The three columns on `notifiers` — last_attempt_at, last_success_at, last_error — are what
// keep a destination that has been failing all week visible rather than silent, which is the
// difference between a stated limitation and a hidden one.
type Dispatcher struct {
	pool    *pgxpool.Pool
	secrets secrets.Provider
	log     *slog.Logger
	cfg     DispatchConfig

	queue chan Message
	wg    sync.WaitGroup

	started   atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}

	// dropped counts notifications the queue had no room for. Reported on Close so that a run that
	// silently lost some says so at least once.
	dropped atomic.Int64

	client *http.Client
	// send is the transport, replaced in tests. Production wires webhook and SMTP.
	send func(ctx context.Context, dest destination, secret string, msg Message) error
	// retryDelay spaces the bounded retry. A field rather than a literal so the unit suite does not
	// spend three real seconds proving that three attempts are three attempts.
	retryDelay time.Duration
}

// NewDispatcher builds one. It does not deliver anything until Start is called.
func NewDispatcher(pool *pgxpool.Pool, secretsProvider secrets.Provider, cfg DispatchConfig, log *slog.Logger) *Dispatcher {
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Attempts <= 0 {
		cfg.Attempts = 3
	}

	d := &Dispatcher{
		pool:    pool,
		secrets: secretsProvider,
		log:     log.With(slog.String("component", "alert-delivery")),
		cfg:     cfg,
		queue:   make(chan Message, cfg.QueueSize),
		closed:  make(chan struct{}),
		client:  &http.Client{Timeout: cfg.Timeout},

		retryDelay: time.Second,
	}
	d.send = d.sendToDestination
	return d
}

// Start begins draining the queue. It returns immediately; Close stops it.
func (d *Dispatcher) Start(ctx context.Context) {
	if !d.started.CompareAndSwap(false, true) {
		return
	}
	for i := 0; i < d.cfg.Workers; i++ {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.worker(ctx)
		}()
	}
}

func (d *Dispatcher) worker(ctx context.Context) {
	for {
		select {
		case <-d.closed:
			return
		case msg, ok := <-d.queue:
			if !ok {
				return
			}
			d.deliver(ctx, msg)
		}
	}
}

// Enqueue offers a notification for delivery.
//
// **A full queue drops rather than blocks.** The caller is the evaluation pass, and a pass that
// waited on an SMTP server would stop finding the other things that are wrong. Losing a
// notification is bad; losing the detection of every subsequent condition because one destination is
// slow is worse, and the alert row is written before this is ever called.
func (d *Dispatcher) Enqueue(ctx context.Context, msg Message) {
	if msg.TenantID == "" {
		return
	}
	select {
	case d.queue <- msg:
	default:
		n := d.dropped.Add(1)
		d.log.WarnContext(ctx, "the notification queue is full; this notification was dropped",
			slog.String("fingerprint", msg.Fingerprint),
			slog.String("state", msg.State),
			slog.Int64("dropped_total", n),
			slog.String("consequence", "the alert row is still there; nobody was told about it"))
	}
}

// Close stops the workers and waits for what is in flight.
func (d *Dispatcher) Close() error {
	d.closeOnce.Do(func() {
		close(d.closed)
		d.wg.Wait()
		if n := d.dropped.Load(); n > 0 {
			d.log.Warn("notifications were dropped because the queue was full",
				slog.Int64("count", n))
		}
	})
	return nil
}

// -----------------------------------------------------------------------------------------------
// One notification
// -----------------------------------------------------------------------------------------------

// destination is one notifier, resolved.
type destination struct {
	ID          string
	TenantID    string
	Name        string
	Kind        string
	Settings    map[string]string
	SecretName  string
	MinSeverity string
}

// deliver sends one message to every destination that wants it.
func (d *Dispatcher) deliver(ctx context.Context, msg Message) {
	dests, err := d.destinationsFor(ctx, msg)
	if err != nil {
		d.log.ErrorContext(ctx, "could not read where to send an alert",
			slog.String("fingerprint", msg.Fingerprint), slog.String("error", err.Error()))
		return
	}
	for _, dest := range dests {
		sendErr := d.attempt(ctx, dest, msg)
		d.recordOutcome(ctx, dest.ID, sendErr)
		if sendErr != nil {
			// The error's own text, never the destination's configuration: a URL can embed a
			// credential and this line is written to a log an operator may ship elsewhere.
			d.log.WarnContext(ctx, "could not deliver an alert notification",
				slog.String("notifier", dest.Name),
				slog.String("notifier_kind", dest.Kind),
				slog.String("fingerprint", msg.Fingerprint),
				slog.String("error", sendErr.Error()),
				slog.String("consequence", "the alert row is unaffected; nobody was told"))
			continue
		}
		d.log.InfoContext(ctx, "alert notification delivered",
			slog.String("notifier", dest.Name),
			slog.String("fingerprint", msg.Fingerprint),
			slog.String("state", msg.State),
			slog.String("severity", msg.Severity))
	}
}

// attempt sends once, retrying a small bounded number of times.
//
// Fixed short delays rather than exponential backoff with jitter. The queue is in memory and bounded
// and Close waits for it, so a retry schedule measured in minutes would either be abandoned at
// shutdown or would hold shutdown open — and an alert delivered fifteen minutes late is a different
// product decision from the one ADR-0039 makes.
func (d *Dispatcher) attempt(ctx context.Context, dest destination, msg Message) error {
	secret, err := d.secretFor(ctx, dest)
	if err != nil {
		return err
	}

	var lastErr error
	for i := 0; i < d.cfg.Attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-d.closed:
				return lastErr
			case <-time.After(time.Duration(i) * d.retryDelay):
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, d.cfg.Timeout)
		lastErr = d.send(attemptCtx, dest, secret, msg)
		cancel()
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (d *Dispatcher) sendToDestination(ctx context.Context, dest destination, secret string, msg Message) error {
	switch dest.Kind {
	case NotifierWebhook:
		return d.sendWebhook(ctx, dest, secret, msg)
	case NotifierSMTP:
		return sendSMTP(ctx, dest, secret, msg)
	default:
		return fmt.Errorf("no transport for notifier kind %q", dest.Kind)
	}
}

// secretFor resolves a notifier's one credential.
//
// The error deliberately carries the reference rather than anything about the value. `Ref.String`
// is documented as containing no secret material, which is why it is safe here and why a provider's
// own error is not wrapped in.
func (d *Dispatcher) secretFor(ctx context.Context, dest destination) (string, error) {
	if dest.SecretName == "" {
		return "", nil
	}
	ref := secrets.Ref{TenantID: dest.TenantID, Name: dest.SecretName}
	plaintext, err := d.secrets.Get(ctx, ref)
	if errors.Is(err, secrets.ErrNotFound) {
		return "", fmt.Errorf("the credential for this notifier (%s) is not in the secrets store", ref)
	}
	if err != nil {
		return "", fmt.Errorf("could not read the credential for this notifier (%s)", ref)
	}
	return string(plaintext), nil
}

// destinationsFor reads the enabled notifiers that want this message.
//
// The severity floor is applied in the query rather than in Go, so a notifier that only wants
// critical alerts costs nothing at all when a warning fires.
func (d *Dispatcher) destinationsFor(ctx context.Context, msg Message) ([]destination, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id::text, name, kind, settings, secret_name, min_severity
		FROM   notifiers
		WHERE  tenant_id = $1 AND is_enabled
		  AND  `+severityRankSQL("min_severity")+` <= $2
		ORDER  BY name`, msg.TenantID, severityRankOf(msg.Severity))
	if err != nil {
		return nil, fmt.Errorf("read notifiers: %w", err)
	}
	defer rows.Close()

	var out []destination
	for rows.Next() {
		dest := destination{TenantID: msg.TenantID}
		if err := rows.Scan(&dest.ID, &dest.Name, &dest.Kind, &dest.Settings,
			&dest.SecretName, &dest.MinSeverity); err != nil {
			return nil, fmt.Errorf("read a notifier: %w", err)
		}
		out = append(out, dest)
	}
	return out, rows.Err()
}

// recordOutcome writes what happened onto the notifier's row.
//
// This is the whole reason at-most-once delivery is defensible. Without it a webhook that has been
// returning 500 for a week is visible only in a log nobody tails, and an operator would have no way
// to tell "nothing is wrong" from "nothing is arriving" — which are the two answers an alerting
// system must never confuse.
func (d *Dispatcher) recordOutcome(ctx context.Context, notifierID string, sendErr error) {
	message := ""
	if sendErr != nil {
		message = truncate(sendErr.Error(), 500)
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE notifiers
		SET    last_attempt_at = now(),
		       last_success_at = CASE WHEN $2 = '' THEN now() ELSE last_success_at END,
		       last_error      = $2,
		       updated_at      = now()
		WHERE  id = $1`, notifierID, message); err != nil {
		d.log.WarnContext(ctx, "could not record a delivery outcome",
			slog.String("notifier_id", notifierID), slog.String("error", err.Error()))
	}
}

// Test sends a real message to one destination and reports what happened.
func (d *Dispatcher) Test(ctx context.Context, tenantID, notifierID string) (bool, string, time.Duration, error) {
	dest, err := d.loadDestination(ctx, tenantID, notifierID)
	if err != nil {
		return false, "", 0, err
	}

	msg := Message{
		TenantID:    tenantID,
		Kind:        "test",
		Severity:    "info",
		State:       "test",
		Summary:     "Fleetward test notification",
		Detail:      "Sent on request. If you are reading this, delivery to " + dest.Name + " works.",
		Fingerprint: "test:" + notifierID,
		FiredAt:     time.Now().UTC(),
	}

	started := time.Now()
	sendErr := d.attempt(ctx, dest, msg)
	took := time.Since(started)

	// Recorded whichever way it went, so a test is as visible afterwards as a real delivery.
	d.recordOutcome(ctx, dest.ID, sendErr)

	if sendErr != nil {
		return false, sendErr.Error(), took, nil
	}
	return true, "", took, nil
}

func (d *Dispatcher) loadDestination(ctx context.Context, tenantID, notifierID string) (destination, error) {
	dest := destination{TenantID: tenantID}
	err := d.pool.QueryRow(ctx, `
		SELECT id::text, name, kind, settings, secret_name, min_severity
		FROM   notifiers
		WHERE  id = $1 AND tenant_id = $2`, notifierID, tenantID).
		Scan(&dest.ID, &dest.Name, &dest.Kind, &dest.Settings, &dest.SecretName, &dest.MinSeverity)
	if errors.Is(err, pgx.ErrNoRows) {
		return destination{}, fmt.Errorf("%w: no notifier %s", ErrNotFound, notifierID)
	}
	if err != nil {
		return destination{}, fmt.Errorf("alerts: read notifier: %w", err)
	}
	return dest, nil
}

// validateSettings refuses a destination that could never be delivered to.
//
// At creation rather than at delivery, because the alternative is discovering a typo in an SMTP
// host during the incident the notifier was configured for.
func validateSettings(kind string, settings map[string]string) error {
	switch kind {
	case NotifierWebhook:
		if settings["url"] == "" {
			return fmt.Errorf("%w: a webhook notifier needs a settings entry named \"url\"",
				ErrInvalidArgument)
		}
	case NotifierSMTP:
		for _, required := range []string{"host", "from", "to"} {
			if settings[required] == "" {
				return fmt.Errorf("%w: an smtp notifier needs a settings entry named %q",
					ErrInvalidArgument, required)
			}
		}
		if mode := settings["tls"]; mode != "" && mode != tlsStartTLS && mode != tlsImplicit && mode != tlsNone {
			return fmt.Errorf("%w: settings.tls must be %q, %q or %q, got %q",
				ErrInvalidArgument, tlsStartTLS, tlsImplicit, tlsNone, mode)
		}
	}
	return nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
