package alerts

import (
	"testing"

	fwv1 "github.com/danmorcov88/fleetward/api/gen/fleetward/v1"
)

// TestInconclusiveVerificationDoesNotFireTheFailedAlert is the enforcement of ADR-0040, and the
// reason this file exists at all.
//
// ADR-0022 separated FAILED from INCONCLUSIVE so that "your backup will not restore" is never muted
// by a flood of "the sandbox could not start". The `verification_failed` evaluator's predicate is
// `status = 'failed'`, and widening it to `status IN ('failed', 'inconclusive')` is a two-word
// change somebody will make in good faith while closing what looks like a gap — after which the
// alert that matters arrives beside a hundred that do not, and gets muted.
//
// A comment is advice. This is a refusal: the test reads the query the evaluator actually runs and
// fails if it ever mentions the other verdict.
func TestInconclusiveVerificationDoesNotFireTheFailedAlert(t *testing.T) {
	// The verdict vocabulary, as the CHECK constraint spells it. If a verdict is added to the schema
	// this list must grow, and that is the moment to decide what it means for alerting.
	const (
		verified     = "verified"
		failed       = "failed"
		inconclusive = "inconclusive"
	)

	tests := []struct {
		name      string
		verdict   string
		wantAlert bool
		why       string
	}{
		{
			name:    "a proven-bad artifact is the alert this product exists to send",
			verdict: failed, wantAlert: true,
			why: "FAILED is evidence about the artifact: it was restored and the data did not match",
		},
		{
			name: "an inconclusive verdict says nothing about the artifact", verdict: inconclusive,
			wantAlert: false,
			why: "INCONCLUSIVE is a sandbox that would not start, a plugin that could not be reached, " +
				"a transfer that broke — none of it evidence that the backup is bad (ADR-0022)",
		},
		{
			name: "a verified backup is not news", verdict: verified, wantAlert: false,
			why: "the ordinary case",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := verdictFires(tc.verdict); got != tc.wantAlert {
				t.Fatalf("a %q verdict fires the verification_failed alert = %v, want %v — %s",
					tc.verdict, got, tc.wantAlert, tc.why)
			}
		})
	}
}

// TestTheEvaluatorQueryNamesOnlyTheFailedVerdict guards the same rule from the other side.
//
// verdictFires below is a statement about intent; this is a statement about the SQL. Together they
// mean the predicate cannot be widened without a test going red, which is the whole mechanism
// ADR-0040 asks for.
func TestTheEvaluatorQueryNamesOnlyTheFailedVerdict(t *testing.T) {
	// The predicate the evaluator applies, extracted so the assertion is about one line rather than
	// about a whole query's text.
	const predicate = "v.status = 'failed'"

	if got := verificationFailedPredicate; got != predicate {
		t.Fatalf("the verification_failed predicate is %q, want %q.\n\n"+
			"If this changed on purpose, read ADR-0022 and ADR-0040 first. An INCONCLUSIVE verdict "+
			"is not evidence about the artifact, and routing it through the same alert as a FAILED "+
			"one is exactly what those decisions forbid: the alert that means a backup will not "+
			"restore arrives beside a hundred that mean Docker was out of disk, and gets muted.",
			got, predicate)
	}
}

func TestFingerprintsIdentifyTheConditionAndNotTheRule(t *testing.T) {
	tests := []struct {
		name string
		c    condition
		want string
	}{
		{
			name: "a failed verification is per backup, so a second bad backup is a second alert",
			c:    condition{kind: KindVerificationFailed, fingerprint: KindVerificationFailed + ":b-1"},
			want: "verification_failed:b-1",
		},
		{
			name: "a missed window is per instance, so tonight's miss updates last night's alert",
			c:    condition{kind: KindBackupMissing, fingerprint: KindBackupMissing + ":i-1"},
			want: "backup_missing:i-1",
		},
		{
			name: "retention is estate-wide: the object store is at fault, not fifty servers",
			c:    condition{kind: KindRetentionBlocked, fingerprint: KindRetentionBlocked + ":estate"},
			want: "retention_blocked:estate",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.fingerprint != tc.want {
				t.Fatalf("fingerprint = %q, want %q", tc.c.fingerprint, tc.want)
			}
			// No rule id anywhere in it. Two overlapping rules describe one broken thing, and an
			// operator woken twice for one broken thing stops trusting the pager.
			if got := tc.c.fingerprint; got != tc.want {
				t.Fatalf("fingerprint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTheLoudestRuleWins(t *testing.T) {
	const instance = "11111111-1111-1111-1111-111111111111"

	tenantWideCritical := ruleRow{id: "r-tenant", kind: KindBackupMissing, severity: "critical"}
	instanceScopedInfo := ruleRow{
		id: "r-instance", kind: KindBackupMissing, severity: "info", instanceID: instance,
	}

	// Both cover the instance. "Most specific wins" would let the narrow `info` rule downgrade the
	// tenant-wide `critical` one, which is a muting mechanism nobody declared and the schema cannot
	// express — the same reasoning that made grants additive in ADR-0034.
	rules := []ruleRow{instanceScopedInfo, tenantWideCritical}
	c := condition{kind: KindBackupMissing, instanceID: instance}

	rule, severity, ok := matchRule(rules, c)
	if !ok {
		t.Fatal("no rule matched a condition two rules cover")
	}
	if severity != "critical" {
		t.Fatalf("severity = %q, want critical: the highest-severity matching rule decides", severity)
	}
	if rule.id != "r-tenant" {
		t.Fatalf("rule_id = %q, want r-tenant: the alert records which rule supplied its severity", rule.id)
	}
}

func TestARuleScopedElsewhereDoesNotMatch(t *testing.T) {
	rules := []ruleRow{
		{id: "r-1", kind: KindInstanceDown, severity: "warning", instanceID: "instance-a"},
		{id: "r-2", kind: KindInstanceDown, severity: "warning", environmentID: "env-a"},
	}

	if _, _, ok := matchRule(rules, condition{
		kind: KindInstanceDown, instanceID: "instance-b", environmentID: "env-b",
	}); ok {
		t.Fatal("a rule scoped to another instance and another environment matched")
	}
	if _, _, ok := matchRule(rules, condition{
		kind: KindInstanceDown, instanceID: "instance-b", environmentID: "env-a",
	}); !ok {
		t.Fatal("an environment-scoped rule did not match an instance in that environment")
	}
}

// TestAnEstateWideConditionNeedsATenantWideRule pins the consequence of retention_blocked having no
// instance: a rule scoped to one server cannot claim it.
func TestAnEstateWideConditionNeedsATenantWideRule(t *testing.T) {
	scoped := []ruleRow{{
		id: "r-1", kind: KindRetentionBlocked, severity: "warning",
		instanceID: "11111111-1111-1111-1111-111111111111",
	}}
	estateWide := condition{kind: KindRetentionBlocked, fingerprint: KindRetentionBlocked + ":estate"}

	if _, _, ok := matchRule(scoped, estateWide); ok {
		t.Fatal("an instance-scoped rule matched an estate-wide condition")
	}

	tenantWide := []ruleRow{{id: "r-2", kind: KindRetentionBlocked, severity: "warning"}}
	if _, _, ok := matchRule(tenantWide, estateWide); !ok {
		t.Fatal("a tenant-wide rule did not match an estate-wide condition")
	}
}

// TestAKindWithNoEvaluatorIsRefused is the other half of "a rule accepted and never evaluated is
// worse than one refused".
func TestAKindWithNoEvaluatorIsRefused(t *testing.T) {
	evaluated := []fwv1.AlertKind{
		fwv1.AlertKind_ALERT_KIND_INSTANCE_DOWN,
		fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED,
		fwv1.AlertKind_ALERT_KIND_BACKUP_FAILED,
		fwv1.AlertKind_ALERT_KIND_BACKUP_MISSING,
		fwv1.AlertKind_ALERT_KIND_RETENTION_BLOCKED,
	}
	for _, kind := range evaluated {
		if !hasEvaluator(kind) {
			t.Errorf("%s is in evaluatedKinds and hasEvaluator says otherwise", kind)
		}
	}

	// Declared in the schema so an operator can see where alerting is going, and refused at creation
	// so nobody believes they are covered by one.
	notYet := []fwv1.AlertKind{
		fwv1.AlertKind_ALERT_KIND_STORAGE_THRESHOLD,
		fwv1.AlertKind_ALERT_KIND_REPLICATION_LAG,
		fwv1.AlertKind_ALERT_KIND_CUSTOM_PROMQL,
	}
	for _, kind := range notYet {
		if hasEvaluator(kind) {
			t.Errorf("%s has no evaluator and hasEvaluator says it does; a rule of that kind would "+
				"be stored and would never fire", kind)
		}
	}

	if hasEvaluator(fwv1.AlertKind_ALERT_KIND_UNSPECIFIED) {
		t.Error("an unspecified kind must not be creatable")
	}
}

// TestEveryEvaluatedKindHasAnEvaluator is the compile-time-ish check that evaluatedKinds and the
// switch in evaluateKind cannot drift apart.
func TestEveryEvaluatedKindHasAnEvaluator(t *testing.T) {
	var s Service
	for _, kind := range evaluatedKinds {
		// A kind on that list with no branch here would be evaluated as nothing and resolved as
		// everything: every alert of that kind would close itself on the next pass, silently.
		if s.evaluatorFor(kind) == nil {
			t.Errorf("%q is in evaluatedKinds and evaluatorFor has no branch for it", kind)
		}
	}
	for _, kind := range []string{KindStorageThreshold, KindReplicationLag, KindCustomPromQL} {
		if s.evaluatorFor(kind) != nil {
			t.Errorf("%q has an evaluator but is not in evaluatedKinds, so it is never run", kind)
		}
	}
}

func TestSeverityOrdering(t *testing.T) {
	if severityRankOf("critical") <= severityRankOf("warning") {
		t.Error("critical must outrank warning")
	}
	if severityRankOf("warning") <= severityRankOf("info") {
		t.Error("warning must outrank info")
	}
	if severityRankOf("nonsense") != 0 {
		t.Error("an unknown severity must rank below every real one rather than above them")
	}
	// A notifier's floor is compared with the same function the SQL expresses, so the two cannot
	// disagree about which alerts a `critical`-only destination receives.
	if severityRank(fwv1.AlertSeverity_ALERT_SEVERITY_CRITICAL) != severityRankOf("critical") {
		t.Error("the enum and the string forms of a severity must rank the same")
	}
}

func TestAVerificationFailedRuleDefaultsToCritical(t *testing.T) {
	if got := defaultSeverityFor(fwv1.AlertKind_ALERT_KIND_VERIFICATION_FAILED); got != "critical" {
		t.Fatalf("verification_failed defaults to %q, want critical: it is the one that means a "+
			"backup will not restore", got)
	}
	if got := defaultSeverityFor(fwv1.AlertKind_ALERT_KIND_BACKUP_MISSING); got != "warning" {
		t.Fatalf("backup_missing defaults to %q, want warning", got)
	}
}

func TestRuleKindRoundTrips(t *testing.T) {
	for _, name := range []string{
		KindInstanceDown, KindVerificationFailed, KindBackupFailed, KindBackupMissing,
		KindStorageThreshold, KindReplicationLag, KindCustomPromQL, KindRetentionBlocked,
	} {
		enum := ruleKindEnum(name)
		if enum == fwv1.AlertKind_ALERT_KIND_UNSPECIFIED {
			t.Errorf("the schema permits kind %q and the contract has no enum value for it", name)
			continue
		}
		back, err := ruleKindString(enum)
		if err != nil {
			t.Errorf("%q maps to %s, which maps back to an error: %v", name, enum, err)
			continue
		}
		if back != name {
			t.Errorf("%q round-tripped to %q", name, back)
		}
	}
}

func TestCredentialShapedSettingsAreRefused(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]string
		wantKey  string
	}{
		{"a webhook token", map[string]string{"url": "https://example", "token": "t"}, "token"},
		{"a password", map[string]string{"host": "mail", "password": "p"}, "password"},
		{"a prefixed key", map[string]string{"smtp_password": "p"}, "smtp_password"},
		{"an api key", map[string]string{"api_key": "k"}, "api_key"},
		{"an authorization header value", map[string]string{"authorization": "Bearer x"}, "authorization"},
		{"the header's name is fine, it is not the value", map[string]string{"auth_header": "X-Token"}, ""},
		{"an ordinary webhook", map[string]string{"url": "https://example"}, ""},
		{"nothing at all", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialShapedKey(tc.settings); got != tc.wantKey {
				t.Fatalf("credentialShapedKey = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

func TestSettingsAreValidatedAtCreationRatherThanAtDelivery(t *testing.T) {
	tests := []struct {
		name     string
		kind     string
		settings map[string]string
		wantErr  bool
	}{
		{"a webhook needs a url", NotifierWebhook, map[string]string{}, true},
		{"a webhook with a url", NotifierWebhook, map[string]string{"url": "https://example"}, false},
		{"smtp needs a host", NotifierSMTP, map[string]string{"from": "a@b", "to": "c@d"}, true},
		{"smtp needs a sender", NotifierSMTP, map[string]string{"host": "mail", "to": "c@d"}, true},
		{"smtp needs a recipient", NotifierSMTP, map[string]string{"host": "mail", "from": "a@b"}, true},
		{
			"a complete smtp destination", NotifierSMTP,
			map[string]string{"host": "mail", "from": "a@b", "to": "c@d"}, false,
		},
		{
			"an unknown tls mode is refused rather than guessed", NotifierSMTP,
			map[string]string{"host": "mail", "from": "a@b", "to": "c@d", "tls": "maybe"}, true,
		},
		{
			"starttls is spelled out", NotifierSMTP,
			map[string]string{"host": "mail", "from": "a@b", "to": "c@d", "tls": tlsStartTLS}, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSettings(tc.kind, tc.settings)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSettings error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
