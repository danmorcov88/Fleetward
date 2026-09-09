# B7 — Alert rules and delivery, or the slice that makes it monitoring

- **Delivered:** 2026-09-09
- **Brief:** [B7-alerts-and-delivery.md](../slices/B7-alerts-and-delivery.md)

Seven slices had built a product that knows things and tells nobody. It knew a verification had
failed, that a backup window had closed empty, that a server had stopped answering, that the object
store had refused every delete for a week — and every one of those facts was reachable only by
somebody deciding to look.

`alert_rules`, `alerts` and `notifiers` had been in the schema since migration `000001`, complete
with a partial unique index on `(tenant_id, fingerprint)` and a comment explaining exactly what it
was for. No code had ever inserted a row against it.

**The deliverable is that five conditions become alert rows on a pass over the estate, that a
condition which keeps being true stays one row, and that the ones which are new reach a webhook or a
mailbox.** The interesting half is the second clause.

## How it was verified

On Windows (amd64), 2026-09-09, against Go 1.25.6, Node 22.16, npm 10.9.2, buf 1.58.0,
golangci-lint 2.12.2, Docker Desktop, `postgres:16-alpine` and
`mcr.microsoft.com/mssql/server:2022-latest`.

```
go build ./...                                    ok
go vet ./...                                      ok
go vet -tags=integration ./...                    ok
go vet -tags=e2e ./...                            ok
go test ./...                                     ok, no failures
go test -tags=integration ./internal/controlplane/alerts/...   ok, 52.6s
go run ./tools/demo -build                        ok, 2m03s, exit 0
buf lint / buf format --diff --exit-code          clean
buf generate + npm run generate                   committed
go run ./tools/docscheck                          90 markdown files, no problems
go run ./tools/docsgen                            no drift after commit
golangci-lint run                                 0 issues   (LF worktree)
npm run lint / npm test                           clean, 22 tests
```

`make` is not installed on this machine, so those are the targets run directly rather than
`make lint test` reported as passing. `go test -race` needs a C toolchain this machine does not
have; CI runs the suites with `-race` on Linux.

**`go vet -tags=integration` earned its place for the third slice running.** A sixth `Runner`
method broke `stubRunner` in `scheduler/integration_test.go`, and a sixth argument to
`scheduler.New` broke three call sites in `scheduler_test.go` — files this slice had no reason to
open, in a package it did.

**The demo found the one defect that mattered, and it was in the demo.** See below.

## What the fingerprint index was waiting for

`idx_alerts_active_fingerprint` is a *partial* unique index — `WHERE state <> 'resolved'` — and
using it turned out to be the whole design in miniature.

```sql
INSERT INTO alerts (...) VALUES (...)
ON CONFLICT (tenant_id, fingerprint) WHERE state <> 'resolved'
DO UPDATE SET last_seen_at = now(), severity = EXCLUDED.severity, ...
RETURNING id, (xmax = 0) AS inserted;
```

Three things about that statement, none of which is obvious and all of which are load-bearing.

**The conflict target repeats the index's predicate.** Without the `WHERE`, PostgreSQL raises "there
is no unique or exclusion constraint matching the ON CONFLICT specification" — a partial index is
not a candidate unless the statement names its predicate. Trivial once known; twenty minutes
otherwise.

**`xmax = 0` is the only honest way to tell an insert from an update.** `RETURNING` cannot otherwise
distinguish them, and `xmax` is a system column that survives `DO UPDATE`. This single expression is
what makes evaluation safe to run with no lease on any number of control planes: of four concurrent
passes over the same condition, exactly one sees `inserted = true`, and only that one delivers. The
integration suite asserts precisely that — four goroutines, one alert, one delivery.

**The `DO UPDATE` branch deliberately does not touch `state`.** `state <> 'resolved'` covers
`acknowledged`, so a condition that is still true updates an acknowledged alert and *leaves it
acknowledged*. Acknowledging silences a condition until it clears; only evaluation clears it,
because only evaluation knows.

Somebody thought all of this through when they wrote migration `000001`. The index and its comment
were right, and the implementation had to be found rather than invented.

## Resolution is by absence, and the naive version had a hole

Each pass computes the fingerprints currently firing; any open alert of a *covered* kind whose
fingerprint is not among them resolves.

The first implementation made "covered" mean "evaluated", which is wrong in a way the integration
suite caught immediately: disabling a rule stopped its kind being evaluated, so its open alerts were
never resolved and stayed firing forever with nothing looking at them. `TestDisablingARuleResolvesItsAlerts`
went red on the first run of the suite.

The correct rule has three cases, and writing them out is what made it obvious:

- a kind with enabled rules whose evaluator ran — **covered**;
- a kind with enabled rules whose evaluator **failed** — *not* covered, because resolving on the
  strength of a query that timed out reports an outage as fixed;
- a kind with no enabled rule at all — **covered, with no fingerprints**, which is exactly what makes
  disabling a rule resolve what it opened.

The middle case is the one that would never have been found by testing the happy path. It is the
difference between "nothing is firing" and "we could not tell", which is the same distinction
ADR-0022 draws about verification verdicts, one layer up.

Deleting a rule needed its own answer: `alerts.rule_id` is `ON DELETE SET NULL`, so a deleted rule
would strand its alerts where no kind covers them. `DeleteAlertRule` resolves them in the same
transaction as the delete, and reports how many.

## The demo found a real bug, and it was the demo's

Act 7 failed on its first full run:

```
demo: act 7, the alert: no webhook naming verification_failed:4508a7b3… arrived within 1m30s;
the alert exists and nobody was told about it
```

The alert had fired, `critical`, with the right fingerprint and the right detail. Nothing arrived.

The instinct was to blame the network — the control plane is a container and the receiver is on the
host, which is exactly the fragile arrangement the brief's traps warned about. A standalone probe
disproved it in two minutes: `docker run --add-host host.docker.internal:host-gateway curl …` posted
to a host listener and it arrived.

The actual cause was the product being right. **Delivery happens on the transition into `firing`,
and act 7 created its notifier long after act 4's verification had already opened the alert.** A
destination configured after an alert opened is correctly told nothing about it; the next pass
updates `last_seen_at` and notifies nobody, which is the entire purpose of the fingerprint.

So the fix is in `run.go`: the receiver and the notifier are created before `actCorruption`, and act
7 says on screen that they were and why. The demo is better for it — the sentence it now prints
teaches the semantics rather than merely showing a webhook.

One more thing came out of that hour, and it is the part worth carrying forward. The act now runs a
`TestNotifier` **pre-flight** before act 4, and that is not belt-and-braces: it is what lets act 7
distinguish *this machine cannot demonstrate delivery* from *the product opened an alert and told
nobody*. The first is an environment fact and the act says so and carries on; the second is a defect
and the act fails. Without the pre-flight the two are the same missing webhook, and the demo would
have had to choose between being flaky and being useless.

That is the same shape as ADR-0022, arrived at from a different direction: a system that cannot tell
"could not" from "did not" ends up reporting both as the more alarming one, and then being ignored.

## FAILED and INCONCLUSIVE, finally doing work

[ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md) was written a slice after
verification worked, to protect a future that did not exist yet: there was no alert, so the
distinction cost nothing and protected nothing.

This is the slice where it starts doing work, and the slice where it can be undone in two words. The
undoing would not look like vandalism — it looks like somebody noticing a week of inconclusive
verifications produced no alert, reading that as a gap, and changing `v.status = 'failed'` to
`v.status IN ('failed', 'inconclusive')` in a commit whose message says "alert on verifications that
did not succeed". A defensible sentence, and the exact failure the ADR exists to prevent.

Three things now stand in the way, and
[ADR-0040](../../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md)
records why all three:

- the predicate is a **named constant**, so widening it is one line in a diff rather than a two-word
  edit inside a fifteen-line string;
- `TestTheEvaluatorQueryNamesOnlyTheFailedVerdict` asserts the constant's value, with a failure
  message that explains what widening costs;
- `TestInconclusiveVerificationDoesNotFireTheFailedAlert` asserts the intent, and an integration
  test asserts it against a real database *and* asserts that a `failed` verdict does fire — so it
  cannot pass because nothing fires at all.

**A comment is advice. A test is a refusal.** The previous decision was written down and would not
have survived on its own; this one is written down and enforced.

The cost is stated rather than hidden: a sandbox provider broken all week means verification is not
happening at all, and nothing pages anybody. That is in `STATUS.md`, and the fix is a separate rule
kind at a lower severity — never a widened predicate.

## Nothing here detects anything new

Worth stating plainly because it is the property that keeps alerting trustworthy. Every evaluator
reads a computation that already answers the same question the API answers:

| Kind | Reads |
|---|---|
| `verification_failed` | `verifications.status` |
| `backup_missing` | `backup.Service.GetBackupAdherence`, the estate view's own function |
| `backup_failed` | the most recent `backups` row per instance |
| `instance_down` | `instances.health`, written by the `discovery` job |
| `retention_blocked` | `backups.state = 'expired'` with `object_key` still set |

So **an alert and the estate screen cannot disagree, because they are the same function.**
`adherence.go:52` anticipated this caller a slice early — "the alert rule that will read this later
reads the same computation rather than a row it would have to trust" — and it turned out to be
exactly right.

The last row deserves a note. Retention already leaves its backlog in rows: `expireOutlivedBackups`
commits the `succeeded → expired` transition on its own, and `deleteExpiredArtifacts` clears
`object_key` only once the object is really gone. `RetentionResult.Unreachable` counts the same thing
for one log line and persists nothing. So the evaluator reads the rows rather than the counter, and
the answer survives a restart — which the counter never did.

## Two things that were nearly wrong

**The kind for the retention condition.** `storage_threshold` was already in the CHECK constraint and
was the obvious place to put it. It is about storage *utilization*, and using it here would have made
the vocabulary an operator writes rules in slightly untrue. Migration `000005` widens the closed set
by exactly one value instead. A kind is not an implementation detail; it is the word a person types.

**Which rule wins when two cover the same thing.** "Most specific wins" is the reflex, and it would
let a narrow `info` rule downgrade a tenant-wide `critical` one — a muting mechanism nobody declared
and the schema cannot express. The highest severity wins instead, which is
[ADR-0034](../../adr/0034-grants-are-additive-and-the-highest-rank-wins.md)'s reasoning about grants
applied one table over. The fingerprint carries no rule id, so one broken thing is one page whatever
the configuration.

## What was deliberately left unbuilt

- **A durable delivery outbox.** Delivery is at-most-once: a bounded queue, a bounded retry, then
  logged and dropped. The trade is defensible only because it is visible, so `notifiers` grew
  `last_attempt_at`, `last_success_at` and `last_error`, `TestNotifier` sends a real message, and
  `docs/ops/alerting.md` says in its own section that the absence of a notification is not evidence
  that nothing is wrong ([ADR-0039](../../adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)).
- **An alert for an `inconclusive` verdict.** Above, and ADR-0040.
- **`storage_threshold`, `replication_lag`, `custom_promql`.** All three want database metrics that
  nothing collects. They stay in the CHECK because that is where alerting is going, and creating one
  is refused rather than stored.
- **Maintenance windows, snoozes, grouping, inhibition.** Disabling a rule is the whole silencing
  story, and the documentation says so rather than implying more.
- **Templating, and a notifier kind per vendor.** One JSON body, one plain-text email. A Slack
  incoming webhook is a webhook.
- **An alerts screen.** `AppShell.tsx` keeps `enabled: false` on `/alerts`.
- **A metric for evaluation itself.** "Did the pass run last night" is the log line and
  `last_success_at`; a counter is B8, which is now next partly because this slice made Fleetward
  something people are supposed to rely on being awake.

## The bill

One migration that widens a CHECK by one value, adds three columns and seeds three rows. Ten RPCs,
all additive, with ten policy entries, one decorator and one line in the coverage test's `services`
list. One package of about 1,900 lines, five evaluators, two transports. One CLI command group. One
demo act. Three ADRs.

Nothing in `internal/controlplane/alerts` reads `instances.engine_type`, and a grep for an engine
name in it finds nothing.
