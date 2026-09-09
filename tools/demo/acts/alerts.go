package acts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// actAlerts is act 7: the alert that fires on the artifact act 4 corrupted.
//
// This is the beat D1 left a slot for. Every claim in it is asserted, and one of them is asserted
// twice from opposite directions, because the interesting half of this act is not that an alert
// fired — it is that only *one* of the two verification verdicts produces one.
//
// The receiver runs in this process and the control plane runs in a container, so compose gives the
// `fleetward` service `host.docker.internal:host-gateway`. Where that does not resolve — an unusual
// daemon, a locked-down runner — the act says so and carries on with the half that does not depend
// on it, rather than pretending a webhook arrived. The delivery path is asserted without any
// networking at all in internal/controlplane/alerts/alerts_integration_test.go, so nothing is
// unproven either way; what changes is which half this screen shows.
func actAlerts(ctx context.Context, n *Narrator, c *client, corrupted backupRow, delivery *alertDelivery) error {
	n.Act(7, "The alert",
		"The corrupted artifact from act 4, and the notification that went out because of it.")

	n.Say("`alert_rules`, `alerts` and `notifiers` have been in the schema since the first " +
		"migration. Until this slice no Go code touched any of them, so a failed verification was " +
		"visible only to somebody who went looking — which is the difference between a dashboard " +
		"and monitoring.")
	n.Say("")

	rules, err := listAlertRules(ctx, c)
	if err != nil {
		return err
	}
	if len(rules) == 0 {
		return fmt.Errorf("a fresh installation has no alert rules, so nothing would ever fire; " +
			"migration 000005 seeds three and this run found none")
	}
	ruleRows := make([][]string, 0, len(rules))
	for _, r := range rules {
		ruleRows = append(ruleRows, []string{
			r.Name,
			strings.ToLower(trimEnum("ALERT_KIND_", r.Kind)),
			strings.ToLower(trimEnum("ALERT_SEVERITY_", r.Severity)),
			"the estate",
		})
	}
	n.Say("What this installation watches for, seeded by the migration and not by the demo:")
	n.Table([]string{"RULE", "KIND", "SEVERITY", "SCOPE"}, ruleRows)

	n.Say("")
	n.Say("Now the alert. Evaluation is a pass over the whole estate on the scheduler's tick — no " +
		"lease, no job row — so this is a matter of waiting rather than of asking.")

	alert, err := awaitAlert(ctx, n, c, "verification_failed:"+corrupted.ID, 2*time.Minute)
	if err != nil {
		return err
	}

	severity := strings.ToLower(trimEnum("ALERT_SEVERITY_", alert.Severity))
	if severity != "critical" {
		return fmt.Errorf("the alert on a backup that failed verification is %q, want critical: a "+
			"backup that will not restore is the loudest thing this product has to say", severity)
	}
	n.Step("alert %s — %s", severity, alert.Summary)
	n.Say("  %s", alert.Detail)
	n.Say("  fingerprint %s", alert.Fingerprint)

	if alert.InstanceID == "" {
		return fmt.Errorf("the alert names no instance, so nobody knows which server to look at")
	}

	// The delivery. The notifier was created before act 4 corrupted the artifact, and that ordering
	// is the product's semantics rather than the demo's convenience: a notification goes out on the
	// transition, so a destination configured *after* an alert opened is correctly told nothing.
	n.Say("")
	if delivery.reachable {
		payload, err := delivery.receiver.await(alert.Fingerprint, 90*time.Second)
		if err != nil {
			return err
		}
		n.Step("a webhook arrived on the host, %d bytes, POST with the credential in a header",
			len(payload.raw))
		n.Say("  %s", payload.text())
		if payload.carriesSecret() {
			return fmt.Errorf("the notifier's credential is in the request body; it belongs in the " +
				"header and nowhere else")
		}
		if err := assertSecretIsNotReadable(ctx, c); err != nil {
			return err
		}
		n.Say("  and GET /api/v1/notifiers returns the destination without its credential")
		n.Say("")
		n.Say("Nobody asked for that. The notifier was configured before act 4 broke anything, " +
			"because a notification goes out on the transition — a destination added after an " +
			"alert has already opened is correctly told nothing about it, and the next pass " +
			"updates one row rather than paging again.")
	} else {
		// Distinguishing "this machine cannot" from "the product did not" is the whole discipline
		// here, and the pre-flight above is what makes the distinction possible.
		n.Say("The webhook half of this act is not shown on this run: %s", delivery.why)
		n.Say("It is asserted with no container networking at all in " +
			"internal/controlplane/alerts/alerts_integration_test.go, so it is proven — just not " +
			"here.")
	}

	// Two passes, one row. This is what `alerts.fingerprint` has existed for since migration
	// 000001, and until this slice nothing had ever inserted against it.
	n.Say("")
	n.Say("The condition is still true, and the next pass finds it again:")
	before := alert.LastSeenAt
	again, err := awaitAlertAdvanced(ctx, c, alert.Fingerprint, before, 90*time.Second)
	if err != nil {
		return err
	}
	count, err := countAlerts(ctx, c, alert.Fingerprint)
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%d alert rows carry fingerprint %s after several passes, want 1: a rule "+
			"that keeps firing must update one row rather than creating thousands", count, alert.Fingerprint)
	}
	n.Step("one row, last seen %s — not a second alert, and not a second notification",
		again.LastSeenAt.UTC().Format("15:04:05Z"))

	// Acknowledging: mutating, audited, and deliberately not the same thing as resolving.
	n.Say("")
	acked, err := acknowledgeAlert(ctx, c, alert.ID)
	if err != nil {
		return err
	}
	if state := trimEnum("ALERT_STATE_", acked.State); state != "ACKNOWLEDGED" {
		return fmt.Errorf("after acknowledging, the alert is %s, want ACKNOWLEDGED", state)
	}
	n.Step("acknowledged — which says \"I know\", never \"it stopped\"")
	n.Say("  only evaluation resolves an alert, because only evaluation knows whether the thing " +
		"is still broken")

	entries, err := auditLog(ctx, c, url.Values{
		"action": {"alert.acknowledge"}, "page_size": {"5"},
	})
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("acknowledging an alert wrote no audit row, and it is a mutating action")
	}
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, []string{
			e.OccurredAt.UTC().Format("15:04:05Z"), e.Actor, e.Action, e.ResourceID,
		})
	}
	n.Say("")
	n.Table([]string{"WHEN", "ACTOR", "ACTION", "ALERT"}, rows)

	n.Say("")
	n.Say("One thing this act deliberately does not show, because it did not happen. Act 4's " +
		"verification came back FAILED and that is what fired this alert. Had it come back " +
		"INCONCLUSIVE — a sandbox that would not start, a plugin that could not be reached — " +
		"nothing would have fired at all, and no notification would have gone anywhere.")
	n.Say("")
	n.Say("That is not an oversight. FAILED is evidence about the artifact and INCONCLUSIVE is " +
		"evidence about the machinery, and routing both through one alert is precisely how the " +
		"alert that means \"this backup will not restore\" arrives beside a hundred that mean " +
		"\"the disk was full\" and gets muted with them. Widening that predicate is a two-word " +
		"change, so three tests exist whose only job is to refuse it — one of them named after " +
		"the mistake.")

	n.Beat()
	return nil
}

// -----------------------------------------------------------------------------------------------
// Setting up delivery, before there is anything to deliver
// -----------------------------------------------------------------------------------------------

// alertDelivery is act 7's receiver and the notifier pointing at it.
type alertDelivery struct {
	receiver   *webhookReceiver
	notifierID string
	reachable  bool
	// why records what stopped delivery being demonstrable, for the sentence act 7 prints instead.
	why string
}

// startAlertDelivery configures a webhook destination before anything has gone wrong.
//
// **The ordering is the product's, not the demo's convenience.** A notification goes out on the
// transition into `firing`, so a destination configured after an alert has already opened is
// correctly told nothing about it. Act 7 can only show a webhook arriving because of act 4 if the
// webhook existed before act 4 ran.
//
// It also settles, before act 4, whether this machine can demonstrate delivery at all. `TestNotifier`
// sends a real message down the real path, so a failure here means the container cannot reach the
// host — an environment fact — while a failure *later* means the product opened an alert and told
// nobody, which is a defect. Act 7 needs that distinction to be able to assert one and excuse the
// other, and only a pre-flight can draw it.
func startAlertDelivery(ctx context.Context, c *client) *alertDelivery {
	receiver, ok := startWebhookReceiver()
	if !ok {
		return &alertDelivery{why: "a listener could not be opened on the host"}
	}

	notifierID, err := createWebhookNotifier(ctx, c, receiver.url)
	if err != nil {
		receiver.Close()
		return &alertDelivery{why: "the notifier could not be created: " + firstLine(err.Error())}
	}

	delivered, message, err := testNotifier(ctx, c, notifierID)
	if err != nil {
		receiver.Close()
		return &alertDelivery{notifierID: notifierID, why: "the test send failed: " + firstLine(err.Error())}
	}
	if !delivered {
		receiver.Close()
		return &alertDelivery{
			notifierID: notifierID,
			why: "the control-plane container cannot reach a listener on this host " +
				"(" + firstLine(message) + "); compose grants it host.docker.internal, and a " +
				"firewall or an unusual daemon can still refuse the route",
		}
	}
	return &alertDelivery{receiver: receiver, notifierID: notifierID, reachable: true}
}

// Close removes the notifier and stops the receiver.
//
// The notifier goes first and it matters: left behind, one pointing at a listener this process is
// about to close would fail every delivery for the rest of the stack's life, and `make demo-keep`
// leaves the stack up on purpose.
func (d *alertDelivery) Close(ctx context.Context, c *client) {
	if d == nil {
		return
	}
	if d.notifierID != "" {
		_ = deleteNotifier(ctx, c, d.notifierID)
	}
	if d.receiver != nil {
		d.receiver.Close()
	}
}

// -----------------------------------------------------------------------------------------------
// The receiver
// -----------------------------------------------------------------------------------------------

// webhookReceiver is an HTTP server on the host that records what the control plane posts to it.
type webhookReceiver struct {
	url    string
	server *http.Server

	mu       sync.Mutex
	received []webhookDelivery
}

type webhookDelivery struct {
	raw    []byte
	header http.Header
	body   map[string]any
}

func (d webhookDelivery) text() string {
	if s, ok := d.body["text"].(string); ok {
		return s
	}
	return string(d.raw)
}

// carriesSecret reports whether the credential leaked into the body. It belongs in a header, where
// a receiver's own request log is far less likely to keep it.
func (d webhookDelivery) carriesSecret() bool {
	return strings.Contains(string(d.raw), demoNotifierSecret)
}

// demoNotifierSecret is the credential the demo gives its notifier. Known, local, and thrown away
// with the receiver — the point is not that it is secret, it is that the product treats it as one.
const demoNotifierSecret = "demo-webhook-token"

// startWebhookReceiver listens on all interfaces so the control plane's container can reach it.
//
// Returns ok=false rather than failing when it cannot listen at all. The demo asserts what it can
// and says what it could not, which is the whole discipline: an act that showed a webhook arriving
// when none did would make every other claim in the demo worth less.
func startWebhookReceiver() (*webhookReceiver, bool) {
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return nil, false
	}

	r := &webhookReceiver{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hook", func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		r.mu.Lock()
		r.received = append(r.received, webhookDelivery{raw: raw, header: req.Header.Clone(), body: body})
		r.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)
	})

	r.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = r.server.Serve(listener) }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = r.server.Close()
		return nil, false
	}
	// The name the daemon resolves to the host from inside a container, granted to the `fleetward`
	// service by docker-compose.yml.
	r.url = fmt.Sprintf("http://host.docker.internal:%d/hook", addr.Port)
	return r, true
}

func (r *webhookReceiver) Close() {
	if r.server != nil {
		_ = r.server.Close()
	}
}

// await waits for a delivery naming one fingerprint.
func (r *webhookReceiver) await(fingerprint string, within time.Duration) (webhookDelivery, error) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, d := range r.received {
			if got, _ := d.body["fingerprint"].(string); got == fingerprint {
				r.mu.Unlock()
				if auth := d.header.Get("Authorization"); auth != demoNotifierSecret {
					return d, fmt.Errorf(
						"the webhook arrived without its credential in the Authorization header, "+
							"which is where a notifier's secret is supposed to go; got %q", auth)
				}
				return d, nil
			}
		}
		r.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
	return webhookDelivery{}, fmt.Errorf(
		"no webhook naming %s arrived within %s; the alert exists and nobody was told about it",
		fingerprint, within)
}

// -----------------------------------------------------------------------------------------------
// The API calls this act makes
// -----------------------------------------------------------------------------------------------

func listAlertRules(ctx context.Context, c *client) ([]alertRuleRow, error) {
	var resp struct {
		Rules []alertRuleRow `json:"rules"`
	}
	if err := c.get(ctx, "/api/v1/alert-rules", nil, &resp); err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	return resp.Rules, nil
}

func createWebhookNotifier(ctx context.Context, c *client, endpoint string) (string, error) {
	var resp struct {
		Notifier *notifierRow `json:"notifier"`
	}
	if err := c.post(ctx, "/api/v1/notifiers", map[string]any{
		"name":     "demo-receiver",
		"kind":     "webhook",
		"settings": map[string]string{"url": endpoint},
		// Never in settings, which the API returns. A settings key that looks like a credential is
		// refused outright, which is the assertion behind this line rather than a convention.
		"secret":       demoNotifierSecret,
		"min_severity": "ALERT_SEVERITY_INFO",
	}, &resp); err != nil {
		return "", fmt.Errorf("create notifier: %w", err)
	}
	if resp.Notifier == nil {
		return "", fmt.Errorf("the control plane created a notifier and returned nothing")
	}
	if !resp.Notifier.HasSecret {
		return "", fmt.Errorf("the notifier was created with a credential and reports having none")
	}
	return resp.Notifier.ID, nil
}

// testNotifier sends a real message down the real path and reports what happened.
//
// A delivery that failed is a successful RPC reporting `delivered: false`, so the error return here
// means the *request* failed rather than the send.
func testNotifier(ctx context.Context, c *client, id string) (bool, string, error) {
	var resp struct {
		Delivered bool   `json:"delivered"`
		Error     string `json:"error"`
	}
	if err := c.post(ctx, "/api/v1/notifiers/"+id+"/test", map[string]any{}, &resp); err != nil {
		return false, "", fmt.Errorf("test notifier: %w", err)
	}
	return resp.Delivered, resp.Error, nil
}

func deleteNotifier(ctx context.Context, c *client, id string) error {
	if _, _, err := c.status(ctx, http.MethodDelete, "/api/v1/notifiers/"+id, nil); err != nil {
		return fmt.Errorf("delete notifier: %w", err)
	}
	return nil
}

// assertSecretIsNotReadable proves the invariant the whole notifier design rests on.
func assertSecretIsNotReadable(ctx context.Context, c *client) error {
	var resp struct {
		Notifiers []notifierRow `json:"notifiers"`
	}
	if err := c.get(ctx, "/api/v1/notifiers", nil, &resp); err != nil {
		return fmt.Errorf("list notifiers: %w", err)
	}
	for _, notifier := range resp.Notifiers {
		for key, value := range notifier.Settings {
			if strings.Contains(value, demoNotifierSecret) {
				return fmt.Errorf("the notifier's credential came back in settings[%q]", key)
			}
		}
	}
	return nil
}

func listAlerts(ctx context.Context, c *client, query url.Values) ([]alertRow, error) {
	var resp struct {
		Alerts []alertRow `json:"alerts"`
	}
	if err := c.get(ctx, "/api/v1/alerts", query, &resp); err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return resp.Alerts, nil
}

func acknowledgeAlert(ctx context.Context, c *client, id string) (alertRow, error) {
	var resp struct {
		Alert *alertRow `json:"alert"`
	}
	if err := c.post(ctx, "/api/v1/alerts/"+id+"/acknowledge", map[string]any{}, &resp); err != nil {
		return alertRow{}, fmt.Errorf("acknowledge alert: %w", err)
	}
	if resp.Alert == nil {
		return alertRow{}, fmt.Errorf("acknowledging returned no alert")
	}
	return *resp.Alert, nil
}

func countAlerts(ctx context.Context, c *client, fingerprint string) (int, error) {
	alerts, err := listAlerts(ctx, c, url.Values{"include_resolved": {"true"}})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, a := range alerts {
		if a.Fingerprint == fingerprint {
			count++
		}
	}
	return count, nil
}

// awaitAlert polls until one fingerprint appears.
func awaitAlert(ctx context.Context, n *Narrator, c *client, fingerprint string, within time.Duration) (alertRow, error) {
	deadline := time.Now().Add(within)
	announced := false
	for time.Now().Before(deadline) {
		alerts, err := listAlerts(ctx, c, nil)
		if err != nil {
			return alertRow{}, err
		}
		for _, a := range alerts {
			if a.Fingerprint == fingerprint {
				return a, nil
			}
		}
		if !announced {
			n.Step("waiting for the next evaluation pass")
			announced = true
		}
		select {
		case <-ctx.Done():
			return alertRow{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return alertRow{}, fmt.Errorf(
		"no alert with fingerprint %s within %s; the verification failed and nothing recorded it",
		fingerprint, within)
}

// awaitAlertAdvanced polls until the alert's last_seen_at moves, which is a later pass finding the
// same condition.
func awaitAlertAdvanced(ctx context.Context, c *client, fingerprint string, since *time.Time, within time.Duration) (alertRow, error) {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		alerts, err := listAlerts(ctx, c, nil)
		if err != nil {
			return alertRow{}, err
		}
		for _, a := range alerts {
			if a.Fingerprint != fingerprint {
				continue
			}
			if since == nil || (a.LastSeenAt != nil && a.LastSeenAt.After(*since)) {
				return a, nil
			}
		}
		select {
		case <-ctx.Done():
			return alertRow{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return alertRow{}, fmt.Errorf(
		"the alert's last_seen_at did not advance within %s, so no later pass found the condition "+
			"still true", within)
}
