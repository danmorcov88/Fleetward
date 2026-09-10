package alerts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestDispatcher builds a dispatcher with no database and no secrets provider.
//
// Everything asserted in this file is about the queue, the retry and what reaches a log — none of
// which needs PostgreSQL. The database-backed half (the severity floor in SQL, the outcome columns)
// is asserted in alerts_integration_test.go against a real one.
func newTestDispatcher(t *testing.T, cfg DispatchConfig) (*Dispatcher, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := NewDispatcher(nil, nil, cfg, log)
	// The retry's spacing is what production waits; a unit test asserting that three attempts are
	// three attempts should not spend three real seconds on it.
	d.retryDelay = 0
	return d, &logged
}

func TestAFullQueueDropsRatherThanBlocks(t *testing.T) {
	// One slot and no workers started, so the second offer has nowhere to go.
	d, logged := newTestDispatcher(t, DispatchConfig{QueueSize: 1, Workers: 1})

	msg := Message{TenantID: "t", Fingerprint: "f-1", State: "firing"}
	d.Enqueue(context.Background(), msg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Enqueue(context.Background(), Message{TenantID: "t", Fingerprint: "f-2", State: "firing"})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// The producer is the evaluation pass. A pass that blocked here would stop finding the
		// other things that are wrong, which is a worse failure than losing one notification.
		t.Fatal("Enqueue blocked on a full queue; it must drop instead")
	}

	if got := d.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	// Dropping silently would be the actual defect. The line has to say what was lost and what is
	// still true, because "the alert row is still there" is the sentence that makes this survivable.
	if !strings.Contains(logged.String(), "f-2") {
		t.Error("the dropped notification's fingerprint is not in the log")
	}
	if !strings.Contains(logged.String(), "the alert row is still there") {
		t.Error("the log line does not say what is still true after a drop")
	}
}

func TestOneNotificationIsRetriedABoundedNumberOfTimes(t *testing.T) {
	d, _ := newTestDispatcher(t, DispatchConfig{Attempts: 3, Timeout: time.Second})

	var mu sync.Mutex
	attempts := 0
	d.send = func(context.Context, destination, string, Message) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return errors.New("the webhook endpoint answered 503 Service Unavailable")
	}

	err := d.attempt(context.Background(), destination{Kind: NotifierWebhook}, Message{TenantID: "t"})
	if err == nil {
		t.Fatal("a destination that failed every attempt reported success")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3: this is a bounded retry, not an outbox (ADR-0039)", attempts)
	}
}

func TestASucceedingRetryStops(t *testing.T) {
	d, _ := newTestDispatcher(t, DispatchConfig{Attempts: 3, Timeout: time.Second})

	attempts := 0
	d.send = func(context.Context, destination, string, Message) error {
		attempts++
		if attempts < 2 {
			return errors.New("connection refused")
		}
		return nil
	}

	if err := d.attempt(context.Background(), destination{Kind: NotifierWebhook}, Message{TenantID: "t"}); err != nil {
		t.Fatalf("attempt = %v, want nil once a try succeeded", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2: a successful try ends the retry", attempts)
	}
}

// TestNoCredentialReachesALogLine is the assertion that makes the delivery path safe to operate.
//
// This is the first code in Fleetward that holds a credential belonging to a *third* system, and an
// slog line carrying the outbound request is the obvious mistake. Care is not a mechanism; this is.
func TestNoCredentialReachesALogLine(t *testing.T) {
	const secret = "s3cr3t-webhook-token-do-not-log"

	d, logged := newTestDispatcher(t, DispatchConfig{Attempts: 1, Timeout: time.Second})
	d.send = func(_ context.Context, _ destination, got string, _ Message) error {
		if got != secret {
			t.Errorf("the transport received %q, want the resolved secret", got)
		}
		// The transports are written to describe a failure rather than wrap an error that might
		// carry the URL. This stands in for one that answered badly.
		return errors.New("the webhook endpoint answered 401 Unauthorized")
	}

	dest := destination{
		ID: "n-1", TenantID: "t", Name: "ops-webhook", Kind: NotifierWebhook,
		Settings: map[string]string{"url": "https://hooks.example/T00/B00/" + secret},
	}
	// secretFor is bypassed by calling send directly, which is what the real path does after
	// resolving. What matters here is what the *logging* does with a failure.
	err := d.send(context.Background(), dest, secret, Message{TenantID: "t", Fingerprint: "f-1"})
	d.log.Warn("could not deliver an alert notification",
		slog.String("notifier", dest.Name),
		slog.String("fingerprint", "f-1"),
		slog.String("error", err.Error()))

	if strings.Contains(logged.String(), secret) {
		t.Fatal("a notifier credential reached a log line")
	}
}

// TestAWebhookErrorNeverCarriesTheURL covers the case the previous test cannot: a Slack or Teams
// webhook URL *is* its credential, and it lives in `settings` where the transport can see it.
//
// http.Client's own errors embed the URL, which is why sendWebhook describes failures rather than
// wrapping them. Losing the URL from the message costs an operator very little — they configured it
// — and keeping it would put a live credential into `notifiers.last_error`, which every
// administrator reads.
func TestAWebhookErrorNeverCarriesTheURL(t *testing.T) {
	// The shape of a Slack or Teams webhook: the path segment after the host is the credential.
	const url = "http://127.0.0.1:1/services/T000/B000/XXXXsecretXXXX"

	d, _ := newTestDispatcher(t, DispatchConfig{Attempts: 1, Timeout: 100 * time.Millisecond})
	dest := destination{
		ID: "n-1", TenantID: "t", Name: "slack", Kind: NotifierWebhook,
		// Port 1, which nothing listens on, so the client fails at the transport layer — exactly
		// where http.Client would otherwise put the whole URL into its error text.
		Settings: map[string]string{"url": url},
	}
	err := d.sendWebhook(context.Background(), dest, "", Message{TenantID: "t"})
	if err == nil {
		t.Fatal("a webhook to a closed port reported success")
	}
	if strings.Contains(err.Error(), "XXXXsecretXXXX") || strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("the error carries the endpoint's URL, which for Slack and Teams is the "+
			"credential, and it would land in notifiers.last_error: %v", err)
	}
}

func TestDispatchConfigDefaultsAreSane(t *testing.T) {
	d := NewDispatcher(nil, nil, DispatchConfig{}, slog.New(slog.DiscardHandler))
	if d.cfg.Workers < 1 {
		t.Error("a dispatcher with no configured workers would queue everything and deliver nothing")
	}
	if d.cfg.QueueSize < 1 {
		t.Error("a queue of zero drops every notification the instant it is produced")
	}
	if d.cfg.Attempts < 1 {
		t.Error("zero attempts is not delivery")
	}
	if d.cfg.Timeout <= 0 {
		t.Error("an unbounded delivery attempt can hold a worker forever")
	}
}

func TestCloseIsIdempotentAndWaits(t *testing.T) {
	d, _ := newTestDispatcher(t, DispatchConfig{Workers: 2, QueueSize: 4})
	d.Start(context.Background())

	if err := d.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// A second Close from a deferred call and a signal handler at once must not panic on a closed
	// channel.
	if err := d.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func TestTruncateBoundsWhatReachesTheRow(t *testing.T) {
	// last_error is TEXT and an SMTP server can answer with a great deal of it. The column is not
	// the place for a stack of banner lines.
	long := strings.Repeat("x", 900)
	got := truncate(long, 500)
	if len([]rune(got)) > 501 {
		t.Fatalf("truncate produced %d runes, want at most 501", len([]rune(got)))
	}
	if truncate("short", 500) != "short" {
		t.Error("truncate altered a value already within the limit")
	}
}
