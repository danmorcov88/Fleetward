//go:build integration

// Integration tests for alert evaluation and delivery against a real metadata store.
//
// Three things in this slice cannot be asserted without PostgreSQL, and all three are the ones that
// decide whether alerting is trustworthy:
//
//   - the upsert against a *partial* unique index, which no fake can reproduce;
//   - `xmax = 0`, which is how one of two control planes learns that it is the one that opened an
//     alert and therefore the one that delivers it;
//   - the row-level scope filter, because a flag on a listing that does not filter would hand a
//     scoped caller the whole estate.
//
// The delivery half runs against httptest rather than a container: nothing about a webhook needs
// Docker, and the point of these tests is what the database does.
//
// Run with: go test -tags=integration ./internal/controlplane/alerts/...
package alerts

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
	"github.com/danmorcov88/fleetward/internal/config"
	"github.com/danmorcov88/fleetward/internal/controlplane/authn"
	"github.com/danmorcov88/fleetward/internal/storage/metadb"
	"github.com/danmorcov88/fleetward/internal/storage/secrets"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	metaImage    = "postgres:16-alpine"
	startTimeout = 3 * time.Minute
	engine       = "testengine"
)

// harness is one test's isolated world: a real database, a stub adherence source, and a dispatcher
// whose transport is recorded rather than sent.
type harness struct {
	svc        *Service
	pool       *pgxpool.Pool
	dispatch   *Dispatcher
	adherence  *stubAdherence
	instance   string
	instanceB  string
	envA, envB string

	mu        sync.Mutex
	delivered []Message
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	ctx, cancel := context.WithTimeout(testCtx(), startTimeout)
	defer cancel()

	pool := startMetaDB(t, ctx)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	h := &harness{pool: pool, adherence: &stubAdherence{}}
	h.envA, h.instance = seedInstance(t, ctx, pool, "production", "prod-1")
	h.envB, h.instanceB = seedInstance(t, ctx, pool, "staging", "stage-1")

	// One provider shared by both, as production has: the service stores a notifier's credential and
	// the dispatcher reads it back, and two providers would let a test pass that production cannot.
	provider := testSecrets(t)

	h.dispatch = NewDispatcher(pool, provider, DispatchConfig{
		Workers: 1, QueueSize: 64, Timeout: 5 * time.Second, Attempts: 1,
	}, log)
	h.dispatch.retryDelay = 0
	// Recorded rather than sent: what these tests assert is *which* transitions produce a
	// notification, which is a question about the upsert rather than about HTTP.
	h.dispatch.send = func(_ context.Context, _ destination, _ string, msg Message) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.delivered = append(h.delivered, msg)
		return nil
	}

	h.dispatch.Start(context.Background())
	t.Cleanup(func() { _ = h.dispatch.Close() })

	// One destination, so a notification has somewhere to go. Without it `destinationsFor` returns
	// nothing and every delivery assertion would pass vacuously — which is the shape of test that
	// keeps passing after delivery stops working.
	h.seedNotifier(t, "everything", "info")

	h.svc = New(pool, provider, h.adherence, h.dispatch, log)
	return h
}

func (h *harness) deliveries() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Message(nil), h.delivered...)
}

// waitForDeliveries polls until n notifications have been recorded, or gives up.
//
// Delivery is asynchronous by design — the evaluation pass hands work to a queue rather than waiting
// on an SMTP server — so a test that read the slice straight after Evaluate would be racing the
// worker rather than asserting anything.
func (h *harness) waitForDeliveries(t *testing.T, n int) []Message {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := h.deliveries()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d notifications were delivered after 10s, want %d", len(got), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seedRule inserts an enabled tenant-wide rule of one kind.
func (h *harness) seedRule(t *testing.T, kind, severity string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(testCtx(), `
		INSERT INTO alert_rules (tenant_id, name, kind, severity)
		VALUES ($1, $2, $3, $4) RETURNING id::text`,
		metadb.DefaultTenantID, "rule-"+kind+"-"+severity, kind, severity).Scan(&id); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	return id
}

// clearSeededRules removes what migration 000005 seeded, so a test starts from a state it declared.
//
// The seeded rules are deliberate product behaviour and asserted in their own test below; every
// other test writes the rules it needs, because a test that depends on a seed is a test that breaks
// when the seed changes for a good reason.
func (h *harness) clearSeededRules(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(testCtx(), `DELETE FROM alert_rules WHERE tenant_id = $1`,
		metadb.DefaultTenantID); err != nil {
		t.Fatalf("clear seeded rules: %v", err)
	}
}

// -------------------------------------------------------------------------------------------------
// The upsert
// -------------------------------------------------------------------------------------------------

// TestARuleThatKeepsFiringUpdatesOneRow is what alerts.fingerprint exists for.
//
// The partial unique index has been in the schema since migration 000001 with a comment saying
// exactly this, and no code had ever inserted a row against it. Ten passes over one broken thing is
// one alert and one notification, not ten of each.
func TestARuleThatKeepsFiringUpdatesOneRow(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	var firstSeen time.Time
	for i := range 10 {
		result, err := h.svc.Evaluate(testCtx())
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		want := 0
		if i == 0 {
			want = 1
		}
		if result.Opened != want {
			t.Fatalf("pass %d opened %d alerts, want %d: after the first, the condition is not new",
				i, result.Opened, want)
		}
		if i == 0 {
			firstSeen = h.lastSeenAt(t, KindInstanceDown+":"+h.instance)
			// The clock has to move for the assertion below to mean anything.
			time.Sleep(10 * time.Millisecond)
		}
	}

	if got := h.countAlerts(t); got != 1 {
		t.Fatalf("ten passes over one broken thing produced %d alert rows, want 1", got)
	}
	if last := h.lastSeenAt(t, KindInstanceDown+":"+h.instance); !last.After(firstSeen) {
		t.Fatalf("last_seen_at did not advance: %s is not after %s", last, firstSeen)
	}
	delivered := h.waitForDeliveries(t, 1)
	// A moment for a second one to arrive if the dedup were broken, so this asserts "exactly one"
	// rather than "at least one".
	time.Sleep(200 * time.Millisecond)
	if got := len(h.deliveries()); got != 1 {
		t.Fatalf("%d notifications were sent for one condition, want 1: this is what stops a "+
			"pager being useless by the third night", got)
	}
	if delivered[0].State != "firing" {
		t.Errorf("the notification's state is %q, want firing", delivered[0].State)
	}
}

// TestOnlyOneOfTwoConcurrentPassesOpensTheAlert is the two-replica case, which is the reason
// evaluation can hold no lease at all.
//
// `xmax = 0` is true only for the tuple that was genuinely inserted. Without it every replica would
// deliver every alert, and an estate with three control planes would page everybody three times for
// one broken backup.
func TestOnlyOneOfTwoConcurrentPassesOpensTheAlert(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		opened  int
		errored error
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := h.svc.Evaluate(testCtx())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errored = err
				return
			}
			opened += result.Opened
		}()
	}
	wg.Wait()

	if errored != nil {
		t.Fatalf("a concurrent pass failed: %v", errored)
	}
	if opened != 1 {
		t.Fatalf("four concurrent passes opened %d alerts in total, want exactly 1", opened)
	}
	if got := h.countAlerts(t); got != 1 {
		t.Fatalf("four concurrent passes produced %d rows, want 1", got)
	}
}

// TestAcknowledgingSurvivesTheNextPass is the distinction between "I know" and "it stopped".
func TestAcknowledgingSurvivesTheNextPass(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	alerts, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("ListAlerts = %d alerts, %v", len(alerts), err)
	}

	acked, err := h.svc.AcknowledgeAlert(testCtx(), alerts[0].GetId())
	if err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}
	if acked.GetState() != fwv1.AlertState_ALERT_STATE_ACKNOWLEDGED {
		t.Fatalf("state after acknowledging = %s, want ACKNOWLEDGED", acked.GetState())
	}

	// The condition is still true, so the next pass finds it and updates the row. It must not
	// reopen it: `state <> 'resolved'` covers `acknowledged`, and the DO UPDATE branch deliberately
	// does not touch `state`.
	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	after, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(after) != 1 || after[0].GetState() != fwv1.AlertState_ALERT_STATE_ACKNOWLEDGED {
		t.Fatalf("an acknowledged alert whose condition is still true came back as %v; "+
			"acknowledging silences a condition until it clears", after)
	}
}

// TestAnAlertResolvesWhenItsConditionDisappears covers resolution by absence, and the notification
// that goes with it.
func TestAnAlertResolvesWhenItsConditionDisappears(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	h.markUp(t, h.instance)

	result, err := h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.Resolved != 1 {
		t.Fatalf("resolved %d, want 1", result.Resolved)
	}

	open, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("%d alerts are still open after the condition cleared", len(open))
	}
	// The row is not deleted: the record that something was broken for a while is the useful part.
	all, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{IncludeResolved: true})
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAlerts(include_resolved) = %d alerts, %v; the resolved row must survive",
			len(all), err)
	}

	// One for the firing, one for the resolution.
	delivered := h.waitForDeliveries(t, 2)
	var resolutions int
	for _, m := range delivered {
		if m.State == "resolved" {
			resolutions++
		}
	}
	if resolutions != 1 {
		t.Fatalf("%d resolution notifications, want 1: an operator told about a fire must be told "+
			"when it goes out", resolutions)
	}
}

// TestDisablingARuleResolvesItsAlerts is the whole silencing story in this version, asserted.
func TestDisablingARuleResolvesItsAlerts(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	ruleID := h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if _, err := h.svc.SetAlertRuleEnabled(testCtx(), ruleID, false); err != nil {
		t.Fatalf("SetAlertRuleEnabled: %v", err)
	}

	// The kind is no longer evaluated, so nothing produces its fingerprint — but the alert must not
	// be left firing forever either. Disabling a rule resolves what it opened.
	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := h.openAlerts(t); got != 0 {
		t.Fatalf("%d alerts are still open after their rule was disabled; \"stop telling me\" has "+
			"to mean the row goes quiet", got)
	}
}

// TestDeletingARuleResolvesItsAlerts covers the orphan `alerts.rule_id ON DELETE SET NULL` creates.
func TestDeletingARuleResolvesItsAlerts(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	ruleID := h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)

	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	resolved, err := h.svc.DeleteAlertRule(testCtx(), ruleID)
	if err != nil {
		t.Fatalf("DeleteAlertRule: %v", err)
	}
	if resolved != 1 {
		t.Fatalf("deleting a rule resolved %d alerts, want 1: without this they would stay firing "+
			"forever with nothing evaluating them", resolved)
	}
	if got := h.openAlerts(t); got != 0 {
		t.Fatalf("%d alerts survived the deletion of their rule", got)
	}
}

// -------------------------------------------------------------------------------------------------
// What is evaluated, and what is not
// -------------------------------------------------------------------------------------------------

// TestAnInconclusiveVerificationOpensNoAlert is ADR-0040 against a real database.
//
// The unit test pins the predicate's text; this one proves what the predicate does. Both, because
// the two can be changed independently and only one of them is obviously about alerting.
func TestAnInconclusiveVerificationOpensNoAlert(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindVerificationFailed, "critical")

	inconclusive := h.seedBackup(t, h.instance, "succeeded")
	h.seedVerification(t, inconclusive, "inconclusive")

	result, err := h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Opened != 0 {
		t.Fatalf("an inconclusive verification opened %d alerts, want 0.\n\n"+
			"An INCONCLUSIVE verdict is a sandbox that would not start, a plugin that could not be "+
			"reached, a transfer that broke — none of it evidence that the backup is bad. Alerting "+
			"on it as though it were a FAILED verdict is what ADR-0022 exists to prevent: the alert "+
			"that means a backup will not restore would arrive beside a hundred that do not.",
			result.Opened)
	}

	// And the other half, so this test cannot pass because nothing fires at all.
	failed := h.seedBackup(t, h.instance, "succeeded")
	h.seedVerification(t, failed, "failed")

	result, err = h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.Opened != 1 {
		t.Fatalf("a failed verification opened %d alerts, want 1", result.Opened)
	}
	alerts, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("%d open alerts, want 1", len(alerts))
	}
	if alerts[0].GetSeverity() != fwv1.AlertSeverity_ALERT_SEVERITY_CRITICAL {
		t.Errorf("severity = %s, want CRITICAL", alerts[0].GetSeverity())
	}
	if alerts[0].GetFingerprint() != KindVerificationFailed+":"+failed {
		t.Errorf("fingerprint = %q, want it to name the backup that failed", alerts[0].GetFingerprint())
	}
}

// TestAnInstanceWithNothingDeclaredIsNotAProblem is how alerting survives its first pass on a fresh
// installation.
func TestAnInstanceWithNothingDeclaredIsNotAProblem(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindBackupMissing, "warning")

	// What GetBackupAdherence answers for an estate nobody has written an expectation for.
	h.adherence.answers = []*fwv1.InstanceAdherence{{
		InstanceId:    h.instance,
		InstanceName:  "prod-1",
		EnvironmentId: h.envA,
		State:         fwv1.AdherenceState_ADHERENCE_STATE_NOT_DECLARED,
	}}

	result, err := h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Opened != 0 {
		t.Fatalf("NOT_DECLARED opened %d alerts, want 0: firing on it would mean an alert per "+
			"instance on the first pass, which is how alerting gets switched off on day one",
			result.Opened)
	}

	h.adherence.answers[0].State = fwv1.AdherenceState_ADHERENCE_STATE_MISSED
	h.adherence.answers[0].Deadline = timestamppb.New(time.Now().Add(-time.Hour))
	result, err = h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if result.Opened != 1 {
		t.Fatalf("a MISSED window opened %d alerts, want 1", result.Opened)
	}
}

// TestMigrationSeedsThreeRules asserts the product behaviour decision 11 of the brief made.
func TestMigrationSeedsThreeRules(t *testing.T) {
	h := newHarness(t)

	rules, err := h.svc.ListAlertRules(testCtx(), true)
	if err != nil {
		t.Fatalf("ListAlertRules: %v", err)
	}
	if len(rules) != 3 {
		t.Fatalf("a fresh database has %d alert rules, want 3: an installation whose alerting has "+
			"to be assembled from nothing before it says anything is one that never gets alerting",
			len(rules))
	}
	byKind := map[fwv1.AlertKind]*fwv1.AlertRule{}
	for _, r := range rules {
		byKind[r.GetKind()] = r
		if !r.GetIsEnabled() {
			t.Errorf("the seeded rule %q is disabled", r.GetName())
		}
		if r.GetInstanceId() != "" || r.GetEnvironmentId() != "" {
			t.Errorf("the seeded rule %q is scoped; the seeds are tenant-wide", r.GetName())
		}
	}
	if got := byKind[fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED]; got == nil ||
		got.GetSeverity() != fwv1.AlertSeverity_ALERT_SEVERITY_CRITICAL {
		t.Error("the seeded verification_failed rule is missing or is not critical")
	}
	for _, kind := range []fwv1.AlertKind{
		fwv1.AlertKind_ALERT_KIND_BACKUP_MISSING,
		fwv1.AlertKind_ALERT_KIND_INSTANCE_DOWN,
	} {
		if byKind[kind] == nil {
			t.Errorf("no seeded rule of kind %s", kind)
		}
	}
}

// TestARuleKindWithNoEvaluatorIsRefused covers the schema permitting more than the code evaluates.
func TestARuleKindWithNoEvaluatorIsRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.CreateAlertRule(testCtx(), CreateRuleInput{
		Name: "storage", Kind: fwv1.AlertKind_ALERT_KIND_STORAGE_THRESHOLD,
	})
	if err == nil {
		t.Fatal("a rule of a kind nothing evaluates was stored; the operator would believe they " +
			"were covered")
	}

	// And one that is evaluated goes in.
	rule, err := h.svc.CreateAlertRule(testCtx(), CreateRuleInput{
		Name: "mine", Kind: fwv1.AlertKind_ALERT_KIND_BACKUP_FAILED,
	})
	if err != nil {
		t.Fatalf("CreateAlertRule: %v", err)
	}
	if rule.GetSeverity() != fwv1.AlertSeverity_ALERT_SEVERITY_WARNING {
		t.Errorf("severity = %s, want the WARNING default", rule.GetSeverity())
	}
}

// -------------------------------------------------------------------------------------------------
// Scope
// -------------------------------------------------------------------------------------------------

// TestListAlertsFiltersItsOwnRows is what the ScopeFiltered flag in Policies promises.
//
// The flag says the method filters. This asserts the filtering, because a flag on a listing that
// does not filter would hand a scoped caller the whole estate (ADR-0035).
func TestListAlertsFiltersItsOwnRows(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindInstanceDown, "warning")
	h.markDown(t, h.instance)
	h.markDown(t, h.instanceB)

	if _, err := h.svc.Evaluate(testCtx()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// A person granted dba on one server. Not a system caller, which sees everything.
	scoped := authn.WithPrincipal(context.Background(), authn.Principal{
		Kind:     authn.KindUser,
		UserID:   "11111111-1111-1111-1111-111111111111",
		Actor:    "dba@example.com",
		TenantID: metadb.DefaultTenantID,
		Grants:   []authn.Grant{{Role: "dba", Rank: 30, InstanceID: h.instance}},
	})

	alerts, err := h.svc.ListAlerts(scoped, ListAlertsInput{})
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("a caller granted one instance saw %d alerts, want 1", len(alerts))
	}
	if alerts[0].GetInstanceId() != h.instance {
		t.Fatalf("the visible alert names %s, want %s", alerts[0].GetInstanceId(), h.instance)
	}

	// And the system caller sees both, which is what makes the evaluator work at all.
	all, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil || len(all) != 2 {
		t.Fatalf("the system caller saw %d alerts, want 2 (err: %v)", len(all), err)
	}
}

// -------------------------------------------------------------------------------------------------
// Delivery
// -------------------------------------------------------------------------------------------------

// TestTheSeverityFloorIsAppliedInTheQuery asserts a notifier only hears what it asked for.
func TestTheSeverityFloorIsAppliedInTheQuery(t *testing.T) {
	h := newHarness(t)
	h.clearNotifiers(t)

	criticalOnly := h.seedNotifier(t, "pager", "critical")
	everything := h.seedNotifier(t, "chat", "info")

	dests, err := h.dispatch.destinationsFor(testCtx(), Message{
		TenantID: metadb.DefaultTenantID, Severity: "warning",
	})
	if err != nil {
		t.Fatalf("destinationsFor: %v", err)
	}
	if len(dests) != 1 || dests[0].ID != everything {
		t.Fatalf("a warning reached %d destinations, want only the one whose floor is info", len(dests))
	}

	dests, err = h.dispatch.destinationsFor(testCtx(), Message{
		TenantID: metadb.DefaultTenantID, Severity: "critical",
	})
	if err != nil {
		t.Fatalf("destinationsFor: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("a critical alert reached %d destinations, want 2", len(dests))
	}
	_ = criticalOnly
}

// TestAFailingNotifierIsVisibleOnItsRow is the whole reason at-most-once delivery is defensible.
//
// Without these columns a webhook that has been answering 500 all week is visible only in a log
// nobody tails, and an operator cannot tell "nothing is wrong" from "nothing is arriving" — the two
// answers an alerting system must never confuse (ADR-0039).
func TestAFailingNotifierIsVisibleOnItsRow(t *testing.T) {
	h := newHarness(t)
	h.clearNotifiers(t)

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()

	id := h.seedWebhookNotifier(t, "ops", failing.URL)

	// The real transport, not the recording stub: what is under test is what a real failure writes.
	h.dispatch.send = h.dispatch.sendToDestination

	delivered, message, _, err := h.svc.TestNotifier(testCtx(), id)
	if err != nil {
		t.Fatalf("TestNotifier: %v", err)
	}
	if delivered {
		t.Fatal("a receiver answering 502 was reported as delivered")
	}
	if message == "" {
		t.Fatal("a failed delivery reported no reason")
	}

	notifiers, err := h.svc.ListNotifiers(testCtx(), true)
	if err != nil {
		t.Fatalf("ListNotifiers: %v", err)
	}
	if len(notifiers) != 1 {
		t.Fatalf("%d notifiers, want 1", len(notifiers))
	}
	n := notifiers[0]
	if n.GetLastError() == "" {
		t.Error("notifiers.last_error is empty after a failed delivery")
	}
	if n.GetLastAttemptAt() == nil {
		t.Error("notifiers.last_attempt_at was not stamped")
	}
	if n.GetLastSuccessAt() != nil {
		t.Error("notifiers.last_success_at was stamped by a delivery that failed")
	}
}

// TestANotifiersCredentialIsNeverReturned is the invariant the whole secrets design rests on.
func TestANotifiersCredentialIsNeverReturned(t *testing.T) {
	const secret = "a-token-nobody-may-read-back"
	h := newHarness(t)

	created, err := h.svc.CreateNotifier(testCtx(), CreateNotifierInput{
		Name: "ops", Kind: NotifierWebhook,
		Settings: map[string]string{"url": "https://example.invalid/hook"},
		Secret:   secret,
	})
	if err != nil {
		t.Fatalf("CreateNotifier: %v", err)
	}
	if !created.GetHasSecret() {
		t.Error("has_secret is false for a notifier created with one")
	}
	for k, v := range created.GetSettings() {
		if v == secret {
			t.Fatalf("the credential came back in settings[%q]", k)
		}
	}

	// And it really is in the secrets store, under this notifier's own name.
	plaintext, err := h.dispatch.secrets.Get(testCtx(), secrets.Ref{
		TenantID: metadb.DefaultTenantID, Name: secretNamePrefix + created.GetId(),
	})
	if err != nil {
		t.Fatalf("the credential is not in the secrets store: %v", err)
	}
	if string(plaintext) != secret {
		t.Fatal("the stored credential does not round-trip")
	}

	// Deleting the notifier takes the credential with it.
	if err := h.svc.DeleteNotifier(testCtx(), created.GetId()); err != nil {
		t.Fatalf("DeleteNotifier: %v", err)
	}
	if _, err := h.dispatch.secrets.Get(testCtx(), secrets.Ref{
		TenantID: metadb.DefaultTenantID, Name: secretNamePrefix + created.GetId(),
	}); err == nil {
		t.Fatal("the credential outlived the notifier that owned it")
	}
}

// TestSettingsThatLookLikeACredentialAreRefused stops the 2am shortcut.
func TestSettingsThatLookLikeACredentialAreRefused(t *testing.T) {
	h := newHarness(t)

	_, err := h.svc.CreateNotifier(testCtx(), CreateNotifierInput{
		Name: "ops", Kind: NotifierWebhook,
		Settings: map[string]string{"url": "https://example.invalid/hook", "token": "shhh"},
	})
	if err == nil {
		t.Fatal("a settings object carrying a token was stored; every administrator listing " +
			"notifiers would be able to read it")
	}
}

// -------------------------------------------------------------------------------------------------
// Retention's backlog
// -------------------------------------------------------------------------------------------------

// TestRetentionBlockedReadsTheSweepsOwnBacklog covers the one estate-wide evaluator.
func TestRetentionBlockedReadsTheSweepsOwnBacklog(t *testing.T) {
	h := newHarness(t)
	h.clearSeededRules(t)
	h.seedRule(t, KindRetentionBlocked, "warning")

	// Freshly expired: the sweep has had one chance and a single failure is a blip.
	h.seedExpiredArtifact(t, h.instance, time.Now().Add(-time.Minute))
	if result, err := h.svc.Evaluate(testCtx()); err != nil || result.Opened != 0 {
		t.Fatalf("a minute-old backlog opened %d alerts (err %v), want 0", result.Opened, err)
	}

	// Older than the six-hour default: the object store is not answering.
	h.seedExpiredArtifact(t, h.instance, time.Now().Add(-24*time.Hour))
	result, err := h.svc.Evaluate(testCtx())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if result.Opened != 1 {
		t.Fatalf("a day-old backlog opened %d alerts, want 1", result.Opened)
	}

	alerts, err := h.svc.ListAlerts(testCtx(), ListAlertsInput{})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("ListAlerts = %d, %v", len(alerts), err)
	}
	// The fault is the object store, not the fifty instances whose artifacts are queued behind it.
	if alerts[0].GetInstanceId() != "" {
		t.Errorf("the estate-wide alert names instance %q", alerts[0].GetInstanceId())
	}
	if alerts[0].GetFingerprint() != KindRetentionBlocked+":estate" {
		t.Errorf("fingerprint = %q, want %s:estate", alerts[0].GetFingerprint(), KindRetentionBlocked)
	}
}

// -------------------------------------------------------------------------------------------------
// Harness plumbing
// -------------------------------------------------------------------------------------------------

type stubAdherence struct {
	answers []*fwv1.InstanceAdherence
}

func (s *stubAdherence) InstanceAdherence(context.Context) ([]*fwv1.InstanceAdherence, error) {
	return s.answers, nil
}

func testCtx() context.Context {
	return authn.WithPrincipal(context.Background(), authn.System("test", metadb.DefaultTenantID))
}

func (h *harness) markDown(t *testing.T, instanceID string) {
	t.Helper()
	h.setHealth(t, instanceID, "HEALTH_STATE_DOWN")
}

func (h *harness) markUp(t *testing.T, instanceID string) {
	t.Helper()
	h.setHealth(t, instanceID, "HEALTH_STATE_UP")
}

func (h *harness) setHealth(t *testing.T, instanceID, health string) {
	t.Helper()
	if _, err := h.pool.Exec(testCtx(),
		`UPDATE instances SET health = $2 WHERE id = $1`, instanceID, health); err != nil {
		t.Fatalf("set health: %v", err)
	}
}

func (h *harness) countAlerts(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(testCtx(),
		`SELECT COUNT(*) FROM alerts WHERE tenant_id = $1`, metadb.DefaultTenantID).Scan(&n); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	return n
}

func (h *harness) openAlerts(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(testCtx(),
		`SELECT COUNT(*) FROM alerts WHERE tenant_id = $1 AND state <> 'resolved'`,
		metadb.DefaultTenantID).Scan(&n); err != nil {
		t.Fatalf("count open alerts: %v", err)
	}
	return n
}

func (h *harness) lastSeenAt(t *testing.T, fingerprint string) time.Time {
	t.Helper()
	var seen time.Time
	if err := h.pool.QueryRow(testCtx(),
		`SELECT last_seen_at FROM alerts WHERE tenant_id = $1 AND fingerprint = $2`,
		metadb.DefaultTenantID, fingerprint).Scan(&seen); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return seen
}

func (h *harness) seedBackup(t *testing.T, instanceID, state string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(testCtx(), `
		INSERT INTO backups (tenant_id, instance_id, origin, state, method_id, bucket, object_key,
		                     started_at, completed_at)
		VALUES ($1, $2, 'managed', $3, 'dump', 'b', 'k-' || gen_random_uuid()::text, now(), now())
		RETURNING id::text`, metadb.DefaultTenantID, instanceID, state).Scan(&id); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	return id
}

func (h *harness) seedVerification(t *testing.T, backupID, status string) {
	t.Helper()
	if _, err := h.pool.Exec(testCtx(), `
		INSERT INTO verifications (tenant_id, backup_id, status, completed_at)
		VALUES ($1, $2, $3, now())`, metadb.DefaultTenantID, backupID, status); err != nil {
		t.Fatalf("seed verification: %v", err)
	}
}

func (h *harness) seedExpiredArtifact(t *testing.T, instanceID string, expiredSince time.Time) {
	t.Helper()
	if _, err := h.pool.Exec(testCtx(), `
		INSERT INTO backups (tenant_id, instance_id, origin, state, method_id, bucket, object_key,
		                     size_bytes, started_at, completed_at, expires_at, updated_at)
		VALUES ($1, $2, 'managed', 'expired', 'dump', 'b', 'k-' || gen_random_uuid()::text,
		        1024, $3, $3, $3, $3)`,
		metadb.DefaultTenantID, instanceID, expiredSince); err != nil {
		t.Fatalf("seed expired artifact: %v", err)
	}
}

func (h *harness) seedNotifier(t *testing.T, name, minSeverity string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(testCtx(), `
		INSERT INTO notifiers (tenant_id, name, kind, settings, min_severity)
		VALUES ($1, $2, 'webhook', '{"url": "https://example.invalid/hook"}'::jsonb, $3)
		RETURNING id::text`, metadb.DefaultTenantID, name, minSeverity).Scan(&id); err != nil {
		t.Fatalf("seed notifier: %v", err)
	}
	return id
}

func (h *harness) seedWebhookNotifier(t *testing.T, name, url string) string {
	t.Helper()
	var id string
	if err := h.pool.QueryRow(testCtx(), `
		INSERT INTO notifiers (tenant_id, name, kind, settings, min_severity)
		VALUES ($1, $2, 'webhook', jsonb_build_object('url', $3::text), 'info')
		RETURNING id::text`, metadb.DefaultTenantID, name, url).Scan(&id); err != nil {
		t.Fatalf("seed webhook notifier: %v", err)
	}
	return id
}

func startMetaDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := postgres.Run(ctx, metaImage,
		postgres.WithDatabase("fleetward"),
		postgres.WithUsername("fleetward"),
		postgres.WithPassword("fleetward-integration"),
		testcontainers.WithWaitStrategy(
			// initdb restarts the server once, so the log line appears twice.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(startTimeout)),
	)
	if err != nil {
		t.Fatalf("start metadata postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate metadata postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	db, err := metadb.Open(ctx, config.MetaDBConfig{DSN: dsn, ConnectTimeout: 30 * time.Second},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db.Pool()
}

func seedInstance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, environment, name string) (environmentID, instanceID string) {
	t.Helper()

	if err := pool.QueryRow(ctx, `
		INSERT INTO environments (tenant_id, name, is_production)
		VALUES ($1, $2, TRUE) RETURNING id::text`,
		metadb.DefaultTenantID, environment).Scan(&environmentID); err != nil {
		t.Fatalf("seed environment: %v", err)
	}
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO instances (tenant_id, environment_id, name, engine_type, host, port)
		VALUES ($1, $2, $3, '%s', 'db.example.internal', 5432) RETURNING id::text`, engine),
		metadb.DefaultTenantID, environmentID, name).Scan(&instanceID); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return environmentID, instanceID
}

// testSecrets builds a real provider over an in-memory store.
//
// The real AES-GCM implementation rather than a stub, because "the credential round-trips through
// the provider and is never returned by an API" is one of the claims these tests make, and a stub
// would let a bug in the reference path pass unnoticed.
func testSecrets(t *testing.T) secrets.Provider {
	t.Helper()
	key := make([]byte, secrets.MasterKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	provider, err := secrets.NewAESGCM(secrets.NewMemoryStore(), key, 1)
	if err != nil {
		t.Fatalf("build secrets provider: %v", err)
	}
	return provider
}

// clearNotifiers removes the destination the harness seeds, for a test that counts them.
func (h *harness) clearNotifiers(t *testing.T) {
	t.Helper()
	if _, err := h.pool.Exec(testCtx(), `DELETE FROM notifiers WHERE tenant_id = $1`,
		metadb.DefaultTenantID); err != nil {
		t.Fatalf("clear notifiers: %v", err)
	}
}
