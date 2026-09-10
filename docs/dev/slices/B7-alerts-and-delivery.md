# B7 — Alert rules and delivery, or the slice that makes it monitoring

Six slices have built a product that knows things. It knows a verification failed, that a backup
window closed with nothing in it, that an instance stopped answering, that the object store has
refused every delete for a week. It tells nobody. Every one of those facts is reachable only by
somebody deciding to look — by polling the REST API, by opening the estate view, by reading the
log.

That is the whole gap this slice closes, and it is the difference between a dashboard and
monitoring:

> **A DBA with fifty servers cannot be the polling loop.**

`alert_rules`, `alerts` and `notifiers` have existed since migration `000001` and no Go code has
ever touched them. `docs/dev/STATUS.md` has said so under "Known broken, or knowingly absent" since
B1. This slice writes that code.

---

## Goal

**A condition Fleetward can already detect becomes an alert row that deduplicates, resolves itself,
and is delivered once to a webhook or an SMTP recipient.**

## Why now

**It is the last thing standing between the product and its own acceptance criteria.** CLAUDE.md
§7.3 requires that "a deliberately corrupted artifact produces `verification failed` + a critical
alert". Slice A6 built the first half and D1 demonstrates it on a real stack every time CI runs.
The words "+ a critical alert" have been unmet since the brief was written.

**The schema has been waiting, and an unread table is a promise nobody is keeping.** `alerts` has a
partial unique index on `(tenant_id, fingerprint) WHERE state <> 'resolved'` — somebody thought
carefully about a rule that keeps firing not creating thousands of rows, and no code has ever
inserted one. `notifiers.secret_name` points into the secrets table for a webhook token that has
never been sent. Three tables, four indexes, and a `CHECK` constraint listing seven rule kinds, all
of it dead.

**Everything it needs to read is already computed.** `GetBackupAdherence` answers "did the backup
happen when it was supposed to" on read, with no stored verdict to go stale — and
`internal/controlplane/backup/adherence.go:52` says in as many words that "the alert rule that will
read this later reads the same computation rather than a row it would have to trust".
`instances.health` is refreshed by the `discovery` job B4 built. `verifications.status`
distinguishes `failed` from `inconclusive`. Retention leaves its own backlog behind in rows. B7 adds
no new detection; it adds a voice.

**It fills act 7 of the demo.** D1's act list was written with the slot left empty on purpose, and
`tools/demo/acts/governance.go:201` says so in a comment. Filling it is an addition, not a rewrite.

**It is the third of the three components the roadmap names.** "What real-time monitoring means
here" lists scheduled health probes (B4, done), a dashboard that refetches (B4, done), and alert
delivery — "the part that makes it monitoring rather than a dashboard".

## Preconditions

All hold on `main` at `6571ff2`. Every one was verified by reading the code, not assumed.

### The schema is there and untouched

- **`alert_rules`** (`internal/storage/metadb/migrations/000001_init.up.sql:354`). `kind` is a
  closed set of seven: `instance_down`, `verification_failed`, `backup_failed`, `backup_missing`,
  `storage_threshold`, `replication_lag`, `custom_promql`. `severity` is
  `info | warning | critical`. `environment_id` and `instance_id` are nullable, and the comment
  above them says "NULL scope columns mean the rule applies tenant-wide".
  `UNIQUE (tenant_id, name)`.
- **`alerts`** (`:378`). `rule_id` is `ON DELETE SET NULL`; `fingerprint` is `NOT NULL` and carries
  the comment "a rule that keeps firing updates one row rather than creating thousands"; `state` is
  `firing | acknowledged | resolved`; `acknowledged_by` references `users`.
- **`CREATE UNIQUE INDEX idx_alerts_active_fingerprint ON alerts (tenant_id, fingerprint) WHERE
  state <> 'resolved'`** (`:399`). This is the load-bearing index and it is **partial** — see the
  traps.
- **`notifiers`** (`:406`). `kind` is `webhook | smtp`. `settings` is JSONB and carries the comment
  "Non-secret settings only. Webhook tokens and SMTP passwords live in the secrets table".
  `secret_name` is `TEXT` defaulting to `''`. `min_severity` defaults to `warning`.
- **`events`** (`:425`) also exists and is also untouched. It is **not** in this slice — see the
  scope fence.
- **No Go file in `internal/`, `cmd/`, `plugins/` or `tools/` reads or writes any of the four.**
  A case-insensitive grep for `alert` returns comments, one CLI help string, and the demo's stub.

### What the evaluators will read already exists

- **`GetBackupAdherence`** (`internal/controlplane/backup/adherence.go:54`) returns
  `[]*fwv1.InstanceAdherence` with an `AdherenceState` of `ADHERENT`, `MISSED`, `UNPROVEN`,
  `FAILED` or `NOT_DECLARED`. It is computed on read and stores nothing.
- **It filters its own rows by the caller's grants** through `authn.VisibilityFor(ctx, 0)`
  (`adherence.go:216`), and **`VisibilityFor` returns `Visibility{All: true}` for a system or
  bootstrap principal** (`internal/controlplane/authn/authn.go:262`). So an evaluator running as
  `system:alerts` sees the whole estate without a special case being written for it.
- **An expectation is declared, never inferred.** `loadExpectations` reads
  `schedules.expected_cron` where the schedule is enabled and the expression is non-empty
  (`adherence.go:250`), which is
  [ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md). An
  instance with nothing declared is `NOT_DECLARED`, which the contract calls "not a problem, and not
  an achievement" — and which must therefore never be an alert.
- **`instances.health`** stores the enum name as text, defaulting to `'HEALTH_STATE_UNKNOWN'`
  (`000001_init.up.sql:78`), beside `health_message` and `last_seen_at`. `HEALTH_STATE_DOWN` is one
  of five values in `api/proto/fleetward/v1/plugin.proto:895`, and the `discovery` job runner writes
  it.
- **`verifications.status`** is `pending | running | verified | failed | inconclusive` (`:305`), and
  the column comment already states why the two failure words are different.
- **The retention sweep leaves its backlog in rows.** `expireOutlivedBackups` commits the
  `succeeded -> expired` transition on its own, and `deleteExpiredArtifacts` clears `object_key`
  only once the object is actually gone (`internal/controlplane/backup/retention.go:222`, `:265`).
  So `state = 'expired' AND object_key <> ''` **is** the queue of artifacts the object store has
  refused — durable, queryable, and already read that way by `previewPendingDeletion` (`:484`).
  `RetentionResult.Unreachable` is a counter for one log line and is persisted nowhere.

### The machinery to hang it on exists

- **The scheduler's tick already does estate-wide work that is not a job.** `tick` runs
  `materialize`, `reap`, `maybeSweepRetention`, `dispatch`
  (`internal/controlplane/scheduler/scheduler.go:222`). `maybeSweepRetention` (`:257`) is the exact
  pattern B7 copies: paced by its own interval, guarded by an `atomic.Bool`, run in a goroutine that
  joins `s.running` so `Close` waits for it, and given its own principal.
- **A system actor is constructed in-process and has no credential.** `authn.System(name, tenantID)`
  (`authn.go:125`) produces `actor = "system:" + name` with an empty `UserID`. The sweep already
  does `authn.System("retention", authn.Tenant(ctx))` (`scheduler.go:265`) precisely so that "who
  deleted this artifact" answers `system:retention` rather than `system:scheduler`
  ([ADR-0036](../../adr/0036-the-scheduler-is-an-actor-and-not-a-user.md)).
- **`audit.Writer.Record` writes `user_id = NULL` and `actor = "system:…"` for a system caller**
  (`internal/controlplane/audit/audit.go`), which is what makes an automatic action auditable at
  all.
- **The secrets provider is an opaque byte store keyed by `Ref{TenantID, Name}`**
  (`internal/storage/secrets/secrets.go`). A notifier credential is exactly the shape it takes.

### The authorization surface will refuse a new RPC until it is written down

One correction to a belief that is easy to carry into this slice, because acting on it wastes a
session:

- **The decorators *do* embed the generated `UnimplementedXServiceServer`.**
  `internal/controlplane/authz/decorators.go:41` embeds it, and the comment above it records that
  the compile-time variant was tried and abandoned: turning `require_unimplemented_servers` off is a
  global switch, and it would break every third-party plugin on any additive change to
  `plugin.proto`. So **adding a method to the contract and forgetting its decorator does not break
  the build.**
- What catches it instead is three things, all real:
  1. a method with no entry in `authz.Policies` is denied to everybody, administrator included —
     fail-closed by construction;
  2. a method the decorator does not override is answered `codes.Unimplemented` by the embedded
     stub and never reaches the real service;
  3. `internal/controlplane/authz/coverage_test.go` enumerates every method of every interface in
     its `services` list by reflection and asserts each answers `Unauthenticated` to an anonymous
     caller — so a forgotten decorator answers `Unimplemented` instead and fails in CI.
- **`services` at `coverage_test.go:41` is a hand-maintained list, and reflection cannot catch a
  service missing from it.** What catches that instead is the reverse direction of the same
  test: `TestEveryRouteHasAPolicy` also walks `MethodNames()` and fails on any policy entry no
  generated interface has, so ten new `AlertService` rules with no `AlertService` in `services`
  fail loudly. *(Corrected during execution: the brief first said the test asserts a method count.
  It does not — the bidirectional check is the guard, and it is a better one.)* Adding
  `AlertServiceServer` to that list is not optional.
- **`ValidatePolicies` runs at startup** and refuses any rule with no action, no resource type, or a
  resource type absent from `resourceIDField` (`guard.go:297`). Three new resource types mean three
  new entries there.

### Everything else worth knowing before starting

- **`docs/dev/data-model.md` and `docs/ops/configuration.md` are generated** by `go run
  ./tools/docsgen`, and CI's `Docs` job fails if they are stale. `tools/docsgen/schema.go:75`
  already lists `alert_rules`, `alerts` and `notifiers` in its section.
- **`docscheck` fails on any repository path named in documentation that does not exist.** A brief
  listing files it will create needs a scoped entry in `docs/.docscheck-allow`, and that entry goes
  stale — reported as unused — the moment the file appears. That is the removal mechanism.
- **The web app's sidebar already carries `{ to: "/alerts", label: "Alerts", enabled: false }`**
  (`web/src/components/AppShell.tsx:24`). It stays disabled — see the scope fence.
- **`docker-compose.yml` has no `extra_hosts` on any service.** The control-plane container cannot
  reach a listener on the developer's host today.
- **The demo's stub is `actAlerts` in `tools/demo/acts/governance.go:207`**, called from
  `tools/demo/acts/run.go:60`. It takes only a `*Narrator` and asserts nothing, which is exactly why
  replacing it is additive.

## Design decisions already made

Not open. Relitigating any of these is the expensive failure mode this section exists to prevent.

### 1. A rule kind maps to an evaluator, and never to an engine

`alert_rules.kind` is a closed set with a `CHECK`. It grows by migration and by nothing else. There
is no rule kind called `postgres_replication_lag`, there never will be, and an evaluator that
branched on `instances.engine_type` would be the exact violation CLAUDE.md §4.1 names. Where an
alert depends on something only some engines can report, the gate is a `Capabilities` field — the
way `storage_threshold` is already described in the generated OpenAPI as available when "storage
utilization metrics are available".

B7 implements five evaluators. Four map onto kinds that already exist; one needs the set widened.

| Kind | Fires when | Default severity |
|---|---|---|
| `verification_failed` | a verification's verdict is `failed` | critical |
| `backup_missing` | adherence for an instance is `MISSED` | warning |
| `backup_failed` | the instance's most recent backup attempt is in state `failed` | warning |
| `instance_down` | `instances.health` is `HEALTH_STATE_DOWN` | warning |
| `retention_blocked` | an artifact has been `expired` with its object still present for longer than the rule's threshold | warning |

`backup_missing` and `backup_failed` are deliberately not the same question. The first needs a
declaration and answers "the window closed empty"; the second needs none and answers "the last
attempt on this instance failed", which is the only one of the two that fires on an instance nobody
has declared an expectation for yet.

`storage_threshold`, `replication_lag` and `custom_promql` keep their place in the `CHECK` and get
no evaluator. Creating a rule of one of those kinds is **refused at creation**, with a message
naming what it waits on — the same treatment `schedules.kind = 'metrics'` already gets. A rule that
is accepted and never evaluated is worse than one that is refused, because the operator believes
they are covered.

### 2. Evaluation is a pass over the estate, not a job — ADR-0038

It runs on the scheduler's tick, beside the retention sweep, on its own interval, holding no lease,
writing no `jobs` row and no `schedules` row. The reasoning is
[ADR-0030](../../adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md)'s, one for one:
the work is estate-wide rather than per-instance, and it is idempotent in a way a backup is not —
the fingerprint index is the transition's own guard, so two control planes evaluating at the same
instant produce one alert rather than a race to lose.

A `jobs` row per rule per minute would also be an unbounded write to a table `ListJobs` reads.

Its actor is `system:alerts`, distinct from `system:scheduler` and `system:retention`, for exactly
the reason ADR-0036 gives: those are different facts, and an audit log that spells them the same way
cannot tell them apart afterwards. The tenant comes from `authn.Tenant(ctx)`. **No tenant constant
appears anywhere in the alerts package.**

### 3. An evaluator reads the same computation the API answers

`backup_missing` calls `backup.Service.GetBackupAdherence` with the system context. It does not
reimplement the window arithmetic and it does not read a stored verdict, because there is no stored
verdict to read — that is the property `adherence.go` was written to have. The consequence worth
naming: an alert and the estate view can never disagree, because they are the same function.

### 4. `FAILED` and `INCONCLUSIVE` do not produce the same alert — ADR-0040

[ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md) exists so that the alert
meaning *your backup will not restore* is never muted by a flood of alerts meaning *Docker was out
of disk*. Routing both verdicts through `verification_failed` would do precisely what that decision
forbids, and it is a two-word change somebody will make in good faith while "closing a gap".

So: **`verification_failed` fires on `status = 'failed'` and on nothing else.** An `inconclusive`
verdict fires no alert in B7. That is a knowing hole, it goes in `STATUS.md` under what is
knowingly absent, and the shape of the eventual fix is a separate rule kind at a lower severity —
never a widened predicate.

The enforcement is a named unit test rather than a comment:
`TestInconclusiveVerificationDoesNotFireTheFailedAlert`. A comment is advice; a test is a refusal.

### 5. The alert row is the record; a notification is best-effort — ADR-0039

There is no `notifications` table in the schema and this slice does not add one. Delivery is
**at-most-once**: a transition is pushed to a bounded in-process queue, sent with a small bounded
retry, and if it still fails it is logged and dropped. The alert row remains, and the API and the
CLI still show it.

The alternative — a durable outbox with its own retry, backoff and poison handling — is a real piece
of engineering, and it is not what stands between this product and being trusted. What matters is
that the operator is told which one they have. So `docs/ops/alerting.md` states in a section of its
own that **the absence of an email is not evidence that nothing is wrong**, and `notifiers` grows
three columns — `last_attempt_at`, `last_success_at`, `last_error` — so a notifier that has been
failing all week is visible in `ListNotifiers` rather than only in the log. That is the same failure
shape retention's `Unreachable` counter exists for, and it gets a better answer here.

### 6. A notification is sent on a transition, and exactly one replica sends it

The upsert is a single statement:

```sql
INSERT INTO alerts (tenant_id, rule_id, instance_id, severity, summary, detail, labels, fingerprint)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, fingerprint) WHERE state <> 'resolved'
DO UPDATE SET last_seen_at = now(),
              severity     = EXCLUDED.severity,
              detail       = EXCLUDED.detail
RETURNING id, (xmax = 0) AS inserted;
```

`xmax = 0` is true only for the tuple that was genuinely inserted, so of two control planes
evaluating the same condition in the same second, exactly one delivers. A rule that keeps firing
updates `last_seen_at` and notifies nobody, which is the entire purpose of the fingerprint.

Delivery happens on two transitions and no others: **firing** (the row was created) and
**resolved**. Acknowledging is not delivered — it is an action taken inside the product by somebody
who is already looking at it.

### 7. A fingerprint identifies the condition, not the rule

`verification_failed:<backup_id>`, `backup_missing:<instance_id>`, `backup_failed:<instance_id>`,
`instance_down:<instance_id>`, `retention_blocked:estate`. Deterministic, replica-independent, and
containing no timestamp.

The rule id is deliberately **not** in it. Two overlapping rules — one tenant-wide, one scoped to an
instance — describe one broken thing, and an operator woken twice for one broken thing stops
trusting the pager. Evaluation therefore considers matching rules in descending severity, the
highest wins, and `alerts.rule_id` records which rule supplied the row.

### 8. An alert resolves itself, and a deleted rule takes its alerts with it

Each pass computes the complete set of fingerprints currently firing for the kinds it evaluated. Any
non-resolved alert whose kind was evaluated and whose fingerprint is not in that set is resolved.
Disabling a rule resolves its open alerts on the next pass: "stop telling me" has to mean the row
goes quiet, not that it is frozen forever.

Deleting a rule sets `alerts.rule_id` to NULL through the foreign key, which would strand its alerts
where nothing evaluates them. So `DeleteAlertRule` resolves the rule's open alerts in the same
transaction as the delete.

### 9. A notifier's credential is one opaque secret and never a settings field

`settings` is JSONB returned by `ListNotifiers`. `secret_name` points into `secrets`, which no API
reads back. For `webhook` the secret is the value of the header named by `settings.auth_header`
(default `Authorization`); for `smtp` it is the password for `settings.username`.

`CreateNotifier` **refuses** a `settings` object containing a key that looks like a credential —
`password`, `token`, `secret`, `api_key`, `authorization` — rather than accepting it and writing a
third party's production credential into a column every admin can read. Enforced by a test, because
the shortcut is exactly the one somebody takes at 2am.

The known limitation, stated in `docs/ops/alerting.md` rather than left to be discovered: a webhook
URL that *embeds* its credential — Slack, Teams — lives in `settings.url`, where an admin listing
notifiers can read it. Notifier management is `admin`-only for that reason among others.

### 10. Who may do what

Straight from what migration `000001` says each seeded role is for: `viewer` has "Read-only access
to inventory, health, backups, and alerts", `operator` "May acknowledge alerts and trigger
discovery", `admin` "Full control".

| RPC | Role | Notes |
|---|---|---|
| `ListAlerts` | viewer | `ScopeFiltered` — filters its own rows, like `ListInstances` |
| `AcknowledgeAlert` | operator | mutating, audited as `alert.acknowledge` |
| `ListAlertRules` | viewer | |
| `CreateAlertRule` | admin | mutating |
| `SetAlertRuleEnabled` | operator | mutating — silencing a rule at 3am is an operator's job |
| `DeleteAlertRule` | admin | mutating |
| `ListNotifiers` | admin | `settings` may embed a credential; see decision 9 |
| `CreateNotifier` | admin | mutating |
| `DeleteNotifier` | admin | mutating |
| `TestNotifier` | admin | mutating — it sends a real message to a real endpoint |

`ListAlerts` carries `ScopeFiltered` and therefore **must** filter, in the service, through
`authn.VisibilityFor`. The flag on a listing that does not filter hands a scoped caller the whole
estate; the integration suite asserts the filtering rather than trusting the flag
([ADR-0035](../../adr/0035-enforcement-is-a-policy-table-and-a-decorator.md)).

### 11. A fresh install alerts on something

Migration `000005` seeds three tenant-wide rules for the default tenant, enabled:
`verification_failed` (critical), `backup_missing` (warning), `instance_down` (warning). No notifier
is seeded, so nothing is delivered anywhere until an operator configures one — the rows appear in
the API and the CLI, and that is all.

An installation whose alerting must be assembled from nothing before it says anything is an
installation that never gets alerting. The three seeded rules are this product's thesis stated as
configuration, and an operator who disagrees disables them in one command.

## Files

### New

| Path | What it is |
|---|---|
| `internal/storage/metadb/migrations/000005_alerts.up.sql` | widen the `kind` CHECK with `retention_blocked`; add `notifiers.last_attempt_at`, `last_success_at`, `last_error`; seed three rules |
| `internal/storage/metadb/migrations/000005_alerts.down.sql` | the reverse |
| `internal/controlplane/alerts/service.go` | rules, notifiers and alerts: list, create, acknowledge, delete |
| `internal/controlplane/alerts/evaluate.go` | the five evaluators, the upsert, the resolve pass |
| `internal/controlplane/alerts/deliver.go` | the bounded dispatcher, the severity floor, secret resolution |
| `internal/controlplane/alerts/webhook.go` | the webhook notifier |
| `internal/controlplane/alerts/smtp.go` | the SMTP notifier |
| `internal/controlplane/alerts/grpc.go` | the gRPC adapter, as every other service has |
| `internal/controlplane/alerts/evaluate_test.go` | fingerprints, dedup, resolution, and ADR-0040's test |
| `internal/controlplane/alerts/deliver_test.go` | severity floor, retry, drop-on-full, no secret in any log line |
| `internal/controlplane/alerts/webhook_test.go` | against `httptest.Server` — no Docker |
| `internal/controlplane/alerts/smtp_test.go` | against an in-process SMTP listener — no Docker |
| `internal/controlplane/alerts/alerts_integration_test.go` | testcontainers: the partial-index upsert, `xmax = 0`, scope filtering |
| `internal/controlplane/scheduler/alertrunner.go` | the adapter, mirroring `retentionrunner.go` |
| `cmd/fleetward-cli/alert.go` | `alert list`, `alert ack`, `alert rule …`, `alert notifier …` |
| `tools/demo/acts/alerts.go` | act 7 |
| `docs/ops/alerting.md` | the reference page |
| `docs/adr/0038-alert-evaluation-is-a-pass-over-the-estate.md` | decision 2 |
| `docs/adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md` | decisions 5 and 6 |
| `docs/adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md` | decision 4 |
| `docs/dev/journal/B7-alerts-and-delivery.md` | the close-out entry |

### Modified

| Path | Why |
|---|---|
| `api/proto/fleetward/v1/controlplane.proto` | `AlertService` and its messages |
| `api/gen/`, `api/openapi/`, `web/src/lib/api.gen.ts` | regenerated — see the traps for Windows |
| `internal/controlplane/authz/policy.go` | ten rules, plus `alert`, `alert_rule` and `notifier` in `resourceIDField` |
| `internal/controlplane/authz/decorators.go` | `GuardAlerts` |
| `internal/controlplane/authz/coverage_test.go` | `AlertServiceServer` in `services`, and the asserted count |
| `internal/config/config.go` | `AlertsConfig` |
| `cmd/fleetward/main.go` | build the service, register it guarded, hand the evaluator to the scheduler |
| `internal/controlplane/scheduler/scheduler.go` | `maybeEvaluateAlerts` on the tick; `Runner` grows one method |
| `docker-compose.yml` | a short evaluation interval for dev, and `extra_hosts` so the demo's receiver is reachable |
| `tools/demo/acts/run.go`, `tools/demo/acts/governance.go` | call the real act; delete the stub |
| `docs/demo.md`, `README.md`, `docs/dev/STATUS.md`, `docs/dev/slices/README.md` | what the product now does |
| `docs/dev/data-model.md`, `docs/ops/configuration.md` | regenerated by `go run ./tools/docsgen` |
| `docs/.docscheck-allow` | scoped entries for the files above, removed as each one appears |

## Reuse, do not rewrite

- **`maybeSweepRetention`** (`internal/controlplane/scheduler/scheduler.go:257`) is the template for
  `maybeEvaluateAlerts`, down to the `atomic.Bool`, the `atomic.Int64` pacing, the `s.running.Add(1)`
  and the per-pass principal. Copy its shape; do not invent a second one.
- **`internal/controlplane/scheduler/retentionrunner.go`** is the template for `alertrunner.go`: the
  scheduler's `Runner` interface stays free of the alerts package's concrete types.
- **`backup.Service.GetBackupAdherence`** — decision 3. Do not write a second window calculation.
- **`authn.VisibilityFor(ctx, 0)`** and the `($2::boolean OR … = ANY($3) OR … = ANY($4))` filter from
  `internal/controlplane/backup/adherence.go:221` — the row-level scope filter, already written
  twice and correct both times.
- **`secrets.Provider.Get` with `secrets.Ref{TenantID, Name}`** — the notifier credential path.
  `internal/controlplane/inventory/credentials.go` is the existing caller to read first.
- **`audit.Writer.Record`** — never assemble a details map from a request message; the package
  comment explains what that cost last time.
- **`guarded[Req, Resp]`** (`internal/controlplane/authz/enforce.go:53`) — every decorator method is
  one line through it.
- **`api.NewGatewayMux`** and the registration shape at `cmd/fleetward/main.go:273` — every service
  is registered wrapped in its guard, deliberately impossible to read without seeing it.
- **`internal/controlplane/identity/`** is the smallest complete service in the tree — service, gRPC
  adapter, policy entries, decorator, CLI. Read it before writing `alerts/`.
- **`tools/demo/acts/narrator.go`** — `Act`, `Say`, `Step`, `Seeded`, `Beat`. Anything seeded goes
  through `Seeded` or it is a fixture passed off as live.

## Traps

- **The unique index is partial, and `ON CONFLICT` will not use it unless you say so.**
  `ON CONFLICT (tenant_id, fingerprint)` alone raises *"there is no unique or exclusion constraint
  matching the ON CONFLICT specification"*. The predicate is part of the conflict target:
  `ON CONFLICT (tenant_id, fingerprint) WHERE state <> 'resolved'`.
- **`xmax = 0` is the only honest way to tell an insert from an update in an upsert.** `RETURNING`
  cannot otherwise distinguish them, and `xmax` is a system column that survives `DO UPDATE`. Get
  this wrong and every replica delivers every alert, which is the failure people uninstall over.
- **An acknowledged alert is not resolved.** `state <> 'resolved'` covers `acknowledged`, so the
  upsert updates such a row and must leave it acknowledged. That is correct: acknowledging silences
  a condition until it clears. Do not reset it to `firing`.
- **`NOT_DECLARED` is not a problem.** `backup_missing` fires only on `MISSED`. An estate where most
  instances have no declared expectation would otherwise produce an alert per instance on the very
  first pass, which is how alerting gets switched off on day one.
- **`ValidatePolicies` runs at startup and refuses an unmapped resource type.** Three new types mean
  three new `resourceIDField` entries, and `alert` maps to `alert_id`, not to `instance_id`. The B6
  walk found audit rows saying `resource_type = instance` beside a *backup's* id; do not add a
  fourth way to do that.
- **`coverage_test.go`'s `services` list is hand-maintained.** Reflection cannot catch a service that
  is not in it, and the count assertion is what catches you.
- **Never log a notifier secret, an SMTP password, or a webhook URL that embeds one.** The delivery
  path is the first code in this product to hold a credential belonging to a *third* system, and an
  `slog` line carrying the outbound request is the obvious mistake. `deliver_test.go` asserts the
  absence rather than trusting care.
- **`net/smtp` has sharp edges.** `SendMail` with a `PlainAuth` refuses to authenticate over an
  unencrypted connection — correct, and it surprises people. Implicit TLS on port 465 needs
  `tls.Dial` plus `smtp.NewClient` rather than `SendMail`. Support the two modes the settings name
  and refuse the rest at creation.
- **Regenerating OpenAPI on Windows corrupts it invisibly.** The generator embeds `.proto` comments
  as YAML strings, so a CRLF checkout writes literal escapes *inside* them, which no line-ending
  normalization touches and `git diff` does not show. Regenerate in a worktree created with
  `git -c core.autocrlf=false worktree add`, and confirm the corruption count is zero
  ([ADR-0029](../../adr/0029-the-openapi-document-is-generated-to-match-the-wire.md)).
- **The demo's webhook receiver runs on the host and the control plane runs in a container.**
  `extra_hosts: ["host.docker.internal:host-gateway"]` on the `fleetward` service is what makes it
  reachable, on Docker Desktop and on Linux alike. **If that turns out not to work on the CI runner,
  do not stage it.** Move the delivery assertion into `alerts_integration_test.go`, which uses
  `httptest` and needs no container networking at all; have act 7 assert firing, dedup,
  acknowledgement and the audit row; and say plainly in `docs/demo.md` which half the demo proves. A
  demo that shows a webhook arriving when none did is worse than an act that shows less.
- **The evaluation interval and the demo's patience are coupled.** The default is 30 seconds, because
  the roadmap's claim is that the answer is "never more than about thirty seconds stale". The demo
  cannot wait 30 seconds twice for one beat, so `docker-compose.yml` sets a shorter interval for the
  development stack and the demo polls with a deadline and narrates the wait.
- **`make` is not installed on this machine, `go test -race` needs a C toolchain it does not have,
  and `.env` carries `FLEETWARD_POSTGRES_PORT=55432`** because a local PostgreSQL holds 5432. Run the
  Makefile's targets directly and say so, rather than reporting `make lint test` as passing.

## Scope fence

Explicitly **not** in this slice. A session reading the roadmap will want all of these.

- **No alerts screen in the web app.** `web/src/components/AppShell.tsx` keeps `enabled: false` on
  `/alerts`. The API and the CLI are the surface; a screen belongs with the rest of the UI work, and
  B4's estate view already renders the failed verification the critical alert is about.
- **No use of the `events` table.** It is a different concept — "an event is a fact that happened, an
  alert is a condition that persists", per its own schema comment — and writing to it would double
  the surface for no additional answer.
- **No `custom_promql`, `storage_threshold` or `replication_lag` evaluator.** All three want
  VictoriaMetrics queries, and nothing collects database metrics yet; that is deferred deliberately
  in the roadmap. The kinds stay in the `CHECK` and creating such a rule is refused with a message
  saying so.
- **No alert for an `inconclusive` verification.** Decision 4 and ADR-0040. It goes in `STATUS.md` as
  knowingly absent.
- **No durable delivery outbox, no per-notifier backoff schedule, no dead-letter queue.** Decision 5.
- **No notification templating.** One JSON body for webhooks, one plain-text body for SMTP, both
  fixed. A template language is a feature request, not a slice.
- **No Slack, PagerDuty, Opsgenie or Teams notifier kind.** `notifiers.kind` is `webhook | smtp`, and
  a Slack incoming webhook is a webhook. A first-class kind per vendor is a later, additive
  migration.
- **No maintenance windows, no silences, no grouping, no inhibition.** Disabling a rule is the whole
  silencing story in B7, and the documentation is honest about it being that.
- **No invented semantics for `for_duration_s`.** The column exists; `retention_blocked` uses
  `threshold` as an age in hours, and the other four evaluators ignore both. Do not give a column
  meaning merely to avoid it being unused.
- **No multi-tenant evaluation loop.** Like every other automatic thing in the product, the pass runs
  for the default tenant. That is a property of the whole product today rather than a new one, and it
  is not this slice's to fix.
- **No self-observability.** "Did the alert pass run" is answered by the log and by `last_success_at`
  on the notifier. A `/metrics` counter is **B8**.

## Done when

Concrete commands and their expected output. `make` is absent on this machine; the direct
equivalents are given.

1. **The contract is regenerated and clean.**
   ```
   buf lint && buf format --diff --exit-code
   buf breaking --against '.git#branch=main'          # additive only: passes
   git diff --exit-code api/ web/src/lib/api.gen.ts   # nothing left to regenerate
   ```
   and the OpenAPI document carries no literal escape sequences inside its embedded comments.

2. **Every new RPC is guarded, by three independent mechanisms.**
   ```
   go test ./internal/controlplane/authz/...
   ```
   passes, including the coverage test, with `AlertServiceServer` added to its `services` list —
   without which its ten new policy entries fail the reverse check for naming methods that no
   generated interface has.

3. **The evaluators are right about the distinction that matters.**
   ```
   go test ./internal/controlplane/alerts/... -run 'Inconclusive|Fingerprint|Resolve|NotDeclared' -v
   ```
   shows `TestInconclusiveVerificationDoesNotFireTheFailedAlert` passing.

4. **Dedup and single delivery hold against a real PostgreSQL.**
   ```
   go test -tags=integration ./internal/controlplane/alerts/...
   ```
   asserts that a condition evaluated ten times produces one row with an advancing `last_seen_at`,
   that two concurrent passes yield exactly one `inserted = true`, and that a scoped caller's
   `ListAlerts` returns only their own instances' alerts.

5. **A webhook and an SMTP message actually go out**, asserted against `httptest.Server` and an
   in-process SMTP listener, with the credential present in the request and absent from every log
   line the test captures.

6. **The whole tree is green.**
   ```
   golangci-lint run
   go run ./tools/docscheck
   go run ./tools/docsgen && git diff --exit-code docs/
   go test ./...
   cd web && npm run lint && npm test
   ```
   `docscheck` reports no unused allowance, which means every `docs/.docscheck-allow` entry this
   brief added has been removed as its file appeared.

7. **The demo tells the story and asserts it.**
   ```
   go run ./tools/demo
   go test -tags=e2e ./test/e2e/...
   ```
   Act 7 prints a critical alert whose fingerprint names the backup act 4 corrupted, shows the
   delivered notification — or says exactly which half is asserted elsewhere, per the trap above —
   shows a second pass creating no second row, acknowledges it as `dba`, and shows the
   `alert.acknowledge` row in the audit log under that user.

8. **The close-out is done.** `docs/dev/STATUS.md` rewritten with B8 next, the alerting entry removed
   from "Known broken" and replaced by the two things B7 knowingly leaves — no alert for an
   inconclusive verification, and at-most-once delivery. Journal entry written with the real numbers.
   `README.md` describes a product that alerts. Three ADRs in `docs/adr/`, linked from CLAUDE.md §2.
