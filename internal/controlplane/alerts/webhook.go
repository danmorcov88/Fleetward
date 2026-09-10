package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// defaultAuthHeader is where a webhook's credential goes when the notifier does not say otherwise.
//
// The header is configurable because "Authorization" is not universal — a receiver may want
// `X-Webhook-Token` or `X-Hub-Signature` — and a fixed header would have meant a notifier kind per
// vendor for a difference of one string.
const defaultAuthHeader = "Authorization"

// webhookPayload is what a webhook receiver gets.
//
// One fixed shape, deliberately. Templating is a feature request rather than a slice, and a payload
// an operator can rewrite is a payload that can be made to leak whatever the template author put in
// it. Everything here is already readable through the API by anyone who can see the alert.
type webhookPayload struct {
	// Version so a receiver can tell a future shape from this one without guessing.
	Version      int    `json:"version"`
	Source       string `json:"source"`
	State        string `json:"state"`
	Kind         string `json:"kind"`
	Severity     string `json:"severity"`
	AlertID      string `json:"alert_id"`
	Fingerprint  string `json:"fingerprint"`
	InstanceID   string `json:"instance_id,omitempty"`
	InstanceName string `json:"instance_name,omitempty"`
	Summary      string `json:"summary"`
	Detail       string `json:"detail"`
	// Also rendered as one line, because a receiver that posts straight into a chat channel wants a
	// sentence rather than a schema.
	Text    string    `json:"text"`
	FiredAt time.Time `json:"fired_at"`
}

// sendWebhook posts one notification.
//
// The credential goes in a header and never in the body, the URL, or a log line. It arrives here as
// a string resolved from the SecretsProvider for this one call and is not retained.
func (d *Dispatcher) sendWebhook(ctx context.Context, dest destination, secret string, msg Message) error {
	url := dest.Settings["url"]
	if url == "" {
		return fmt.Errorf("this webhook notifier has no url in its settings")
	}

	body, err := json.Marshal(webhookPayload{
		Version:      1,
		Source:       "fleetward",
		State:        msg.State,
		Kind:         msg.Kind,
		Severity:     msg.Severity,
		AlertID:      msg.AlertID,
		Fingerprint:  msg.Fingerprint,
		InstanceID:   msg.InstanceID,
		InstanceName: msg.InstanceName,
		Summary:      msg.Summary,
		Detail:       msg.Detail,
		Text:         oneLine(msg),
		FiredAt:      msg.FiredAt,
	})
	if err != nil {
		return fmt.Errorf("could not encode the notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// Never the URL: it may embed a credential, which is the limitation docs/ops/alerting.md
		// states rather than leaves to be discovered.
		return fmt.Errorf("this webhook notifier's url could not be used")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "fleetward")
	if secret != "" {
		header := dest.Settings["auth_header"]
		if header == "" {
			header = defaultAuthHeader
		}
		req.Header.Set(header, secret)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		// http.Client puts the URL in its error text, so the error is described rather than
		// wrapped. A notifier whose URL is its credential must not leak it into last_error.
		return fmt.Errorf("the webhook endpoint could not be reached")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the webhook endpoint answered %s", resp.Status)
	}
	return nil
}

// oneLine renders a notification as a sentence, for a receiver that shows text rather than JSON.
func oneLine(msg Message) string {
	var b strings.Builder
	switch msg.State {
	case "resolved":
		b.WriteString("RESOLVED")
	case "test":
		b.WriteString("TEST")
	default:
		b.WriteString(strings.ToUpper(msg.Severity))
	}
	b.WriteString(" — ")
	b.WriteString(msg.Summary)
	if msg.Detail != "" {
		b.WriteString(" ")
		b.WriteString(msg.Detail)
	}
	return b.String()
}
