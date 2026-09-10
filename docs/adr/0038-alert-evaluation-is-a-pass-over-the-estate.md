# ADR-0038: Alert evaluation is a pass over the estate, not a job

- **Status:** Accepted
- **Date:** 2026-09-09
- **Slice:** B7 — alert rules and delivery
- **Relates to:** [ADR-0013](0013-internal-scheduler-with-leases.md),
  [ADR-0028](0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md),
  [ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
  [ADR-0036](0036-the-scheduler-is-an-actor-and-not-a-user.md),
  [ADR-0039](0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)

## Context

`alert_rules`, `alerts` and `notifiers` had been in the schema since migration `000001` and no Go
code had ever touched them. Writing that code meant answering the same two mechanical questions
[ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md) answered for retention, plus
one the retention sweep never had to face.

**What kind of work is an evaluation?** Every recurring thing Fleetward does is a `schedules` row
that materializes into a leased `jobs` row ([ADR-0013](0013-internal-scheduler-with-leases.md)).
Alerting does not fit that mould for the same reason retention did not: a rule may be scoped to an
instance, but *a pass* answers "what in this estate is currently true", not "what should happen to
this instance".

**Where does the answer come from?** Every condition B7 alerts on was already computed somewhere. A
missed backup window is `GetBackupAdherence`, which is computed on read and stores no verdict. A
failed verification is `verifications.status`. A silent server is `instances.health`, refreshed by
the `discovery` job. A retention sweep that cannot delete anything is `backups.state = 'expired'`
with an `object_key` still on the row. So the choice was between reading those and materializing a
parallel set of "alert state" tables that would then have to be kept in step with them.

**And the one retention did not face: two control planes must not page everybody twice.** Retention
is idempotent because the state transition is its own guard and object deletion is idempotent by the
object store's contract. Alerting has an outward-facing side effect — a webhook, an email — and
sending it twice is not harmless.

## Decision

### 1. Evaluation runs on the scheduler's tick, holds no lease, and writes no job row

`Runner` gains an `EvaluateAlerts` method, the second thing on that interface that is not a job. The
scheduler calls it from `tick`, paced by `FLEETWARD_ALERTS_EVAL_INTERVAL` rather than by the poll
interval, in a goroutine that joins the same wait group as every running job so `Close` waits for it.
The shape is copied from `maybeSweepRetention` deliberately, down to the `atomic.Bool` guard.

**No lease, because the fingerprint index is the guard.** `alerts` carries
`CREATE UNIQUE INDEX idx_alerts_active_fingerprint ON alerts (tenant_id, fingerprint) WHERE state <>
'resolved'`, which has been in the schema since migration `000001` with a comment saying exactly
what it is for. An upsert against it matches a given condition for exactly one of two concurrent
passes, and the other sees an update. A lease would be protecting against a collision the database
already refuses.

**No job row, additionally, because there would be so many of them.** A pass per rule per thirty
seconds is an unbounded write to a table `ListJobs` reads, on an estate that has a handful of rules
and fifty instances. Retention's argument was that it is estate-wide; this one's is that plus
frequency.

### 2. The default interval is thirty seconds, because that number is already published

`docs/roadmap.md` states what real-time monitoring means here: "the answer to 'does anything need my
attention right now' is never more than about thirty seconds stale, and finding out does not require
looking at the screen." Thirty seconds is therefore not a tuning choice — it is the claim the
roadmap already makes, and a default that did not honour it would make that sentence false.

### 3. An evaluator reads the computation the API already answers

`backup_missing` calls `backup.Service.GetBackupAdherence` rather than reimplementing the window
arithmetic. `verification_failed` reads `verifications.status`. `instance_down` reads
`instances.health`. `retention_blocked` reads the sweep's own backlog of expired rows whose objects
are still present.

The consequence worth naming: **an alert and the estate view can never disagree, because they are
the same function.** `internal/controlplane/backup/adherence.go` anticipated this caller in a comment
written a slice earlier — "the alert rule that will read this later reads the same computation rather
than a row it would have to trust" — and this is that caller.

### 4. A rule kind selects an evaluator and never an engine

`alert_rules.kind` is a closed set with a `CHECK` constraint. It grows by migration and by nothing
else. Nothing in `internal/controlplane/alerts` reads `instances.engine_type`, and a grep for an
engine name in that package should find nothing (CLAUDE.md §4.1).

Migration `000005` widens the set by exactly one value, `retention_blocked`, because the condition is
real and none of the seven existing kinds described it. `storage_threshold` is about storage
utilization and would have been the wrong word.

Three kinds — `storage_threshold`, `replication_lag`, `custom_promql` — keep their place in the
`CHECK` and have no evaluator. Creating a rule of one of them is **refused**, with a message naming
what it waits on. A rule that is accepted and never evaluated is worse than one that is refused,
because the operator believes they are covered and finds out during the incident it was meant to
catch. This is the same treatment `schedules.kind = 'metrics'` already gets.

### 5. Resolution is by absence, scoped to the kinds the pass actually covered

Each pass computes the complete set of fingerprints currently firing. Any non-resolved alert of a
covered kind whose fingerprint is not in that set is resolved.

"Covered" is deliberately not the same list as "evaluated", and the difference is the correctness of
the whole mechanism:

- a kind with enabled rules whose evaluator ran is covered, so a condition that cleared closes its
  alert;
- a kind with enabled rules whose evaluator **failed** is *not* covered, because closing its alerts
  on the strength of a query that timed out would report an outage as fixed;
- a kind with no enabled rule at all is covered with no fingerprints, which is what makes disabling
  a rule resolve what it opened. "Stop telling me" has to mean the row goes quiet rather than that
  it freezes where it was.

Deleting a rule sets `alerts.rule_id` to NULL through the foreign key, which would strand its alerts
where nothing evaluates them. `DeleteAlertRule` therefore resolves them in the same transaction as
the delete.

### 6. A pass carries a system actor of its own

`system:alerts`, distinct from `system:scheduler` and `system:retention`, for the reason
[ADR-0036](0036-the-scheduler-is-an-actor-and-not-a-user.md) gives: those are different facts, and an
audit log that spells them the same way cannot tell them apart afterwards. The tenant comes from the
principal on the context. No tenant constant appears anywhere in the alerts package, and a pass
carrying no principal fails its first query rather than quietly reading a default tenant.

## Consequences

**Good.**

- An alert and the estate view cannot drift, because there is one computation behind both.
- Two control planes evaluating the same estate produce one alert and one notification.
- A rule kind that nothing evaluates cannot be stored, so the API cannot lie about coverage.
- Adding an evaluator is a function, a constant in `evaluatedKinds`, and a branch in `evaluatorFor` —
  and a test asserts those two cannot drift apart.

**Costs, accepted.**

- **`job list` cannot answer "did evaluation run last night."** The same consequence retention has,
  and the answers are the same: the log line, and the rows themselves.
- **Evaluation is per-tenant only in the sense the rest of the product is.** The pass runs for the
  tenant on its principal, which today is always the default one. That is a property of the whole
  product rather than a new one.
- **An evaluator that is slow slows the pass.** Five estate-wide queries on a thirty-second cadence
  is not a load problem on fifty instances; on five thousand it would be, and the fix then is a
  cursor rather than a lease.

## Alternatives considered

**A `schedules.kind = 'alerts'` row per tenant, leased like every other job.** Uniform with
[ADR-0013](0013-internal-scheduler-with-leases.md), and wrong for the same reasons retention's would
have been, plus one more: a `jobs` row every thirty seconds is 2,880 rows a day in a table an
operator reads to find out what happened to their backups.

**Materialized alert-state tables, reconciled after every backup and verification.** Faster to
evaluate and a permanent source of "the screen and the pager disagree". The whole reason adherence is
computed on read ([ADR-0028](0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md))
is that there is then nothing to be stale, and alerting is the last place to give that up.

**A lease around the pass, for symmetry with jobs.** It would have made the `xmax = 0` mechanism in
[ADR-0039](0039-the-alert-is-the-record-and-the-notification-is-best-effort.md) unnecessary — and it
would also have meant that a control plane holding the lease when it died stopped all alerting until
the lease expired. Alerting is the component least able to afford a single point of failure.

**Evaluating every kind on every pass, regardless of whether a rule asks about it.** Simpler, and it
would mean an estate with three rules running five estate-wide queries. The cost is trivial today and
the discipline is not: an evaluator nobody asked for is work nobody consented to.
