package alerts

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// received is what a test receiver saw.
type received struct {
	method  string
	headers http.Header
	body    []byte
}

// webhookReceiver stands in for whatever an operator points a notifier at.
//
// httptest rather than a container: nothing about this needs Docker, and a transport test that
// needs a container is a transport test that stops being run.
func webhookReceiver(t *testing.T, status int) (*httptest.Server, <-chan received) {
	t.Helper()
	got := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{method: r.Method, headers: r.Header.Clone(), body: body}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestAWebhookCarriesTheAlertAndTheCredential(t *testing.T) {
	const secret = "Bearer a-token-the-receiver-checks"

	srv, got := webhookReceiver(t, http.StatusOK)
	d := NewDispatcher(nil, nil, DispatchConfig{Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))

	msg := Message{
		TenantID: "t", AlertID: "a-1", Kind: KindVerificationFailed, Severity: "critical",
		State: "firing", InstanceID: "i-1", InstanceName: "prod-orders",
		Summary:     "A backup of prod-orders failed verification",
		Detail:      "The restored data did not match its manifest.",
		Fingerprint: "verification_failed:b-1",
		FiredAt:     time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC),
	}
	dest := destination{
		TenantID: "t", Name: "ops", Kind: NotifierWebhook,
		Settings: map[string]string{"url": srv.URL},
	}

	if err := d.sendWebhook(context.Background(), dest, secret, msg); err != nil {
		t.Fatalf("sendWebhook = %v", err)
	}

	r := <-got
	if r.method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.method)
	}
	if h := r.headers.Get("Authorization"); h != secret {
		t.Errorf("Authorization = %q, want the notifier's secret", h)
	}
	if ct := r.headers.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var payload webhookPayload
	if err := json.Unmarshal(r.body, &payload); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if payload.Fingerprint != msg.Fingerprint {
		t.Errorf("fingerprint = %q, want %q", payload.Fingerprint, msg.Fingerprint)
	}
	if payload.Severity != "critical" || payload.State != "firing" {
		t.Errorf("severity/state = %q/%q, want critical/firing", payload.Severity, payload.State)
	}
	if payload.Version != 1 {
		t.Errorf("version = %d, want 1: a receiver has to be able to tell a future shape from this one",
			payload.Version)
	}
	// The credential must be in the header and nowhere else. A body carrying it would land in a
	// receiver's own request log, which is a place Fleetward has no control over.
	if strings.Contains(string(r.body), secret) {
		t.Error("the notifier's credential is in the request body")
	}
	// A receiver that posts straight into a chat channel wants a sentence, not a schema.
	if !strings.Contains(payload.Text, "CRITICAL") || !strings.Contains(payload.Text, "prod-orders") {
		t.Errorf("text = %q, want a one-line rendering naming the severity and the instance", payload.Text)
	}
}

func TestAWebhookWithNoCredentialSendsNoAuthorizationHeader(t *testing.T) {
	srv, got := webhookReceiver(t, http.StatusOK)
	d := NewDispatcher(nil, nil, DispatchConfig{Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))

	dest := destination{TenantID: "t", Kind: NotifierWebhook, Settings: map[string]string{"url": srv.URL}}
	if err := d.sendWebhook(context.Background(), dest, "", Message{TenantID: "t"}); err != nil {
		t.Fatalf("sendWebhook = %v", err)
	}
	if h := (<-got).headers.Get("Authorization"); h != "" {
		t.Errorf("Authorization = %q, want none: a notifier with no secret sends no credential", h)
	}
}

func TestAWebhookCredentialGoesInTheConfiguredHeader(t *testing.T) {
	srv, got := webhookReceiver(t, http.StatusOK)
	d := NewDispatcher(nil, nil, DispatchConfig{Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))

	// The header is configurable because "Authorization" is not universal, and a fixed one would
	// have meant a notifier kind per vendor for a difference of one string.
	dest := destination{
		TenantID: "t", Kind: NotifierWebhook,
		Settings: map[string]string{"url": srv.URL, "auth_header": "X-Webhook-Token"},
	}
	if err := d.sendWebhook(context.Background(), dest, "opaque", Message{TenantID: "t"}); err != nil {
		t.Fatalf("sendWebhook = %v", err)
	}
	r := <-got
	if h := r.headers.Get("X-Webhook-Token"); h != "opaque" {
		t.Errorf("X-Webhook-Token = %q, want the secret", h)
	}
	if h := r.headers.Get("Authorization"); h != "" {
		t.Errorf("Authorization = %q, want none when another header was named", h)
	}
}

func TestANonSuccessStatusIsAFailure(t *testing.T) {
	srv, _ := webhookReceiver(t, http.StatusInternalServerError)
	d := NewDispatcher(nil, nil, DispatchConfig{Timeout: 5 * time.Second}, slog.New(slog.DiscardHandler))

	dest := destination{TenantID: "t", Kind: NotifierWebhook, Settings: map[string]string{"url": srv.URL}}
	err := d.sendWebhook(context.Background(), dest, "", Message{TenantID: "t"})
	if err == nil {
		t.Fatal("a receiver that answered 500 was treated as delivery")
	}
	// The status is the one thing an operator needs and the one thing that cannot be a credential.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

func TestAResolutionIsMarkedAsOne(t *testing.T) {
	msg := Message{State: "resolved", Severity: "critical", Summary: "prod-orders is answering again"}
	if line := oneLine(msg); !strings.HasPrefix(line, "RESOLVED") {
		t.Fatalf("oneLine = %q, want it to lead with RESOLVED: a receiver that cannot tell a "+
			"resolution from a new alert shows the same red banner twice", line)
	}
}
