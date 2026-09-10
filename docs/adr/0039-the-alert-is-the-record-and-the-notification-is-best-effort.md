# ADR-0039: The alert row is the record, and the notification is best-effort

- **Status:** Accepted
- **Date:** 2026-09-09
- **Slice:** B7 — alert rules and delivery
- **Relates to:** [ADR-0009](0009-secrets-provider-interface.md),
  [ADR-0024](0024-production-readiness-is-a-slice-property.md),
  [ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
  [ADR-0038](0038-alert-evaluation-is-a-pass-over-the-estate.md)

## Context

Once evaluation records an alert, something has to leave the process and tell somebody. That is the
first code in Fleetward that reaches out to a *third party's* system — not a monitored database, not
our own object store, but whatever endpoint an operator configured — and three questions came with
it.

**How durable is a notification?** The industry answer is an outbox: a `notifications` table, a
delivery attempt per row, exponential backoff, a dead-letter queue for what never succeeds. That is a
real piece of engineering with its own failure modes, its own operational surface, and its own
pruning question.

**What happens when two control planes evaluate the same condition in the same second?**
[ADR-0038](0038-alert-evaluation-is-a-pass-over-the-estate.md) established that the fingerprint index
makes the *row* idempotent. It says nothing about the webhook.

**Where does a notifier's credential live?** `notifiers.settings` is JSONB returned by
`ListNotifiers`. `notifiers.secret_name` points into the `secrets` table, which no API reads back.
The schema has carried a comment about that since migration `000001`, and a comment is not a
mechanism.

## Decision

### 1. There is no notifications table, and delivery is at-most-once

A transition is pushed onto a bounded in-process queue, sent with a small bounded retry, and if it
still fails it is logged, recorded on the notifier's row, and dropped. The alert row remains, and the
API and the CLI still show it.

The trade is stated rather than hidden. **The alert row is the record; the notification is a
convenience.** An outbox would make a notification durable across a control-plane restart, and it is
not what stands between this product and being trusted: the thing an operator must never lose is the
knowledge that a backup will not restore, and that is a row in PostgreSQL either way.

[ADR-0024](0024-production-readiness-is-a-slice-property.md) requires a capability to ship with its
limits. Three of them are shipped here.

**`notifiers` grows `last_attempt_at`, `last_success_at` and `last_error`.** This is what makes the
trade defensible rather than merely convenient. Without them a webhook that has been answering 500
all week is visible only in a log nobody tails, and an operator cannot tell "nothing is wrong" from
"nothing is arriving" — the two answers an alerting system must never confuse.

**`TestNotifier` sends a real message to a real endpoint.** There are two moments to discover that a
destination is misconfigured: now, or during the incident it was configured for.

**`docs/ops/alerting.md` says outright that the absence of a notification is not evidence that
nothing is wrong.** In its own section, not in a footnote.

### 2. A full queue drops rather than blocks

The producer is the evaluation pass. A pass that waited on a slow SMTP server would stop finding the
*other* things that are wrong, which is a worse failure than losing one notification — and the alert
row is written before the queue is ever touched. Every drop is logged with the fingerprint and with
the sentence that makes it survivable: the alert row is still there.

### 3. Exactly one control plane delivers, and `xmax = 0` is how it knows

The upsert is one statement:

```sql
INSERT INTO alerts (...) VALUES (...)
ON CONFLICT (tenant_id, fingerprint) WHERE state <> 'resolved'
DO UPDATE SET last_seen_at = now(), severity = EXCLUDED.severity, ...
RETURNING id, (xmax = 0) AS inserted;
```

Two details, neither obvious.

**The conflict target repeats the index's predicate**, because `idx_alerts_active_fingerprint` is a
*partial* unique index and PostgreSQL otherwise raises "there is no unique or exclusion constraint
matching the ON CONFLICT specification".

**`xmax = 0` is true only for the tuple that was genuinely inserted.** `RETURNING` cannot otherwise
distinguish an insert from an update, and `xmax` is a system column that survives `DO UPDATE`. So of
two control planes evaluating the same condition in the same second, exactly one delivers.

A notification goes out on two transitions and no others: **firing** (the row was created) and
**resolved**. A rule that keeps firing updates `last_seen_at` and notifies nobody, which is the whole
purpose of the fingerprint. Acknowledging is not delivered — it is an action taken inside the product
by somebody who is already looking at it.

### 4. A fingerprint identifies the condition, not the rule

`verification_failed:<backup_id>`, `backup_missing:<instance_id>`, `instance_down:<instance_id>`,
`retention_blocked:estate`. Deterministic, replica-independent, and containing no timestamp.

The rule id is deliberately absent. Two overlapping rules — one tenant-wide, one scoped to an
instance — describe one broken thing, and an operator woken twice for one broken thing stops trusting
the pager. Matching rules are considered in descending severity, the highest wins, and
`alerts.rule_id` records which one supplied the row.

"Most specific wins" was the alternative and is wrong for the reason
[ADR-0034](0034-grants-are-additive-and-the-highest-rank-wins.md) gives about grants: it would turn a
narrow `info` rule into a way of *downgrading* a tenant-wide `critical` one, which is a muting
mechanism nobody declared and the schema cannot express.

### 5. A notifier's credential is one opaque secret, and settings are checked for one

The credential goes through the `SecretsProvider` ([ADR-0009](0009-secrets-provider-interface.md))
under `notifier/<uuid>` and never into `settings`. For a webhook it becomes the value of the header
named by `settings.auth_header`, defaulting to `Authorization`; for SMTP it is the password for
`settings.username`.

`CreateNotifier` **refuses** a settings object containing a key that looks like a credential —
`password`, `token`, `secret`, `api_key`, `authorization`. The schema's comment asked for this and
could not enforce it; the check can, and the shortcut it prevents is exactly the one somebody takes
at 2am.

Two smaller rules follow from the same concern, and both are asserted by tests rather than left to
care:

- **No credential reaches a log line.** The transports describe failures rather than wrapping errors
  that carry them — `http.Client` puts the URL in its error text, and for a Slack or Teams webhook
  that URL *is* the credential, so an unwrapped error would land it in `notifiers.last_error`, which
  every administrator reads.
- **`smtp.PlainAuth` refuses to authenticate over an unencrypted connection.** That is correct and it
  surprises people, so a notifier with a username and `tls: none` is refused with a message saying
  which of the two to change, rather than failing inside the standard library during an outage.

## Consequences

**Good.**

- No table to prune, no backoff schedule to tune, no dead-letter queue to operate.
- A notification is sent once per transition per estate, however many control planes there are.
- A failing destination is visible in the API rather than only in a log.
- One overlapping-rule configuration produces one page.

**Costs, accepted and written down.**

- **A notification can be lost.** A control plane that restarts with work in its queue loses it; a
  destination that is down for the whole retry window is never told. The alert row survives all of
  it, and `docs/ops/alerting.md` says so where an operator will read it.
- **A webhook URL that embeds its own credential is readable by an administrator.** Slack and Teams
  build the token into the path, and that path lives in `settings.url`, which `ListNotifiers`
  returns. Notifier management is `admin`-only partly for this reason, and the limitation is stated
  rather than left to be discovered.
- **A queue that fills drops.** Bounded by `FLEETWARD_ALERTS_DELIVERY_QUEUE_SIZE`, counted, and
  reported on shutdown.

## Alternatives considered

**A durable outbox with backoff and a dead-letter queue.** The correct answer for a product whose
notifications are the record. Here they are not: the alert row is, and it is durable already. Deferred
rather than rejected forever — the shape of the change is a table and a drainer, and nothing in this
decision makes it harder later.

**Delivering on every pass while a condition holds, rather than on the transition.** Removes the need
for `xmax = 0` entirely and produces one page every thirty seconds for as long as a backup is broken.
That is how alerting gets muted, which is the failure this whole slice is trying to avoid.

**An advisory lock around delivery instead of `xmax = 0`.** Works, and adds a lock to a path that a
single query already makes correct.

**Putting the credential in `settings` and redacting it on read.** Simpler to write, and it puts a
third party's live credential in a JSONB column where every future query, log line, and debug dump
would carry it. The redaction would be one `if` away from being forgotten.
