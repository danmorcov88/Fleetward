# Alerting

How Fleetward decides that something needs your attention, what it does about it, and — the part
most alerting documentation leaves out — what it will not tell you.

Every setting named here is documented in [`configuration.md`](configuration.md). The decisions
behind the design are [ADR-0038](../adr/0038-alert-evaluation-is-a-pass-over-the-estate.md),
[ADR-0039](../adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md) and
[ADR-0040](../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md).

---

## The one thing to read before you rely on this

**The absence of a notification is not evidence that nothing is wrong.**

Delivery is at-most-once and best-effort. A webhook that was down when an alert fired is not retried
an hour later; a control plane that restarted with a notification still in its queue lost it. The
alert row survives all of that, and `fleetward-cli alert list` will still show it.

That is a deliberate trade and not an oversight
([ADR-0039](../adr/0039-the-alert-is-the-record-and-the-notification-is-best-effort.md)). It is
defensible only because you can see delivery failing:

```
fleetward-cli alert notifier list
```

reports when each destination was last tried, when it last succeeded, and what went wrong. A
destination whose LAST OK is "never" and whose LAST ERROR names a status code has been silently
failing, and that column is how you find out.

## What Fleetward alerts on

Evaluation is a pass over the whole estate on the scheduler's tick, every
`FLEETWARD_ALERTS_EVAL_INTERVAL` (thirty seconds by default). It holds no lease and writes no job
row, so `fleetward-cli job list` will not show it; the log line and the alert rows are the record.

Five conditions have evaluators:

| Kind | Fires when | Reads |
|---|---|---|
| `verification_failed` | a backup was restored into a sandbox and the data did not match its manifest | `verifications.status` |
| `backup_missing` | a backup was expected inside a declared window and none arrived | the same adherence computation the estate view uses |
| `backup_failed` | the most recent backup attempt on an instance failed | `backups.state` |
| `instance_down` | an instance stopped answering its health probe | `instances.health` |
| `retention_blocked` | expired artifacts the object store will not let us delete | `backups.state = 'expired'` with an object still present |

Nothing here detects anything new. Every evaluator reads a computation that already answers a
question the API answers, which is why an alert and the estate view can never disagree.

**`backup_missing` needs a declaration.** It fires on an instance whose schedule carries an
`--expected-cron`, and on no other. An instance with nothing declared is `NOT_DECLARED`, which is
not a problem and not an achievement
([ADR-0028](../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md)). If you
want to be told about a server nobody has written an expectation for yet, `backup_failed` is the rule
that needs no declaration.

**`retention_blocked` is one alert for the estate, not one per artifact.** The fault is the object
store, not the fifty instances whose artifacts are queued behind it. Its `threshold` is an age in
hours and defaults to six: the sweep runs hourly, one failure is a blip, and six consecutive ones is
a store that is not answering.

### What it does not alert on, and why

**An `inconclusive` verification produces no alert.** A sandbox that never became ready, a plugin
that could not be reached, a transfer that broke — none of it is evidence about the backup, and
routing it through the same alert as a `failed` verdict is how the alert that means *this backup will
not restore* gets muted ([ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md),
[ADR-0040](../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md)).

The cost is real and is stated in `docs/dev/STATUS.md`: a sandbox provider that has been broken all
week means verification is not happening at all, and nothing pages you about it. The estate view
shows the verdicts and readiness reports the sandbox provider as degraded; neither is a page.

**`storage_threshold`, `replication_lag` and `custom_promql` exist in the schema and have no
evaluator.** All three want database metrics, and nothing collects those yet. Creating a rule of one
of those kinds is **refused** rather than stored — a rule that is accepted and never fires is worse
than one that is refused, because you would believe you were covered.

## Rules

A rule says which condition is worth being told about, at what severity, over which part of the
estate. A fresh installation has three, seeded by migration `000005` and enabled:

```
fleetward-cli alert rule list

NAME                        KIND                 SEVERITY  SCOPE       ENABLED
Backup verification failed  verification_failed  critical  the estate  true
Backup window missed        backup_missing       warning   the estate  true
Instance down               instance_down        warning   the estate  true
```

An installation whose alerting has to be assembled from nothing before it says anything is one that
never gets alerting, which is why those three exist. No notifier is seeded, so nothing is delivered
anywhere until you configure one.

```
fleetward-cli alert rule create \
  --name "Backups failing in production" \
  --kind backup_failed \
  --severity critical \
  --environment production
```

A rule with neither `--instance` nor `--environment` covers the whole tenant.

**Two rules covering the same broken thing produce one alert, at the higher of the two severities.**
Nobody is woken twice for one problem. The alert records which rule supplied its severity.

That is deliberately not "most specific wins", which would let a narrow `info` rule *downgrade* a
tenant-wide `critical` one — a muting mechanism nobody declared, and the same reasoning that makes
grants additive ([ADR-0034](../adr/0034-grants-are-additive-and-the-highest-rank-wins.md)).

### Silencing

Disabling a rule is the whole silencing story in this version, and this page is honest about it being
that: there are no maintenance windows, no per-alert snoozes, and no inhibition rules.

```
fleetward-cli alert rule disable <rule-id>
```

A disabled rule's open alerts are resolved on the next evaluation pass. "Stop telling me" has to mean
the row goes quiet rather than that it freezes where it was.

## Alerts

```
fleetward-cli alert list
```

Open alerts, loudest first. A caller granted a few instances sees those instances' alerts rather than
a refusal. An estate-wide alert — `retention_blocked` — belongs to no server, so only somebody who
can see the whole tenant sees it.

**A condition that keeps being true updates one row.** `alerts.fingerprint` identifies the condition,
not the rule that found it, so ten passes over one broken backup produce one alert row with an
advancing `last_seen_at` and exactly one notification. This is what stops a pager being useless by
the third night.

**An alert resolves itself.** When the condition stops being true, the next pass marks the alert
resolved and sends one more notification saying so. The row is kept: that something was broken for
six hours on Tuesday is the useful part.

```
fleetward-cli alert ack <alert-id>
```

**Acknowledging says "I know", never "it stopped".** It stops delivery and leaves the alert open;
only evaluation resolves one, because only evaluation knows whether the thing is still broken. It is
a mutating action and lands in `audit_log` under your name.

## Notifiers

A notifier is a webhook or an SMTP destination. Delivery goes to every enabled notifier whose
`--min-severity` the alert meets.

### Webhook

```
FLEETWARD_NOTIFIER_SECRET='Bearer a-token-the-receiver-checks' \
fleetward-cli alert notifier create \
  --name ops-webhook \
  --kind webhook \
  --setting url=https://alerts.internal.example/fleetward \
  --min-severity warning
```

| Setting | Meaning |
|---|---|
| `url` | where to POST. Required. |
| `auth_header` | which header carries the credential. Defaults to `Authorization`. |

The body is one fixed JSON shape, versioned so a receiver can tell a future one from this:

```json
{
  "version": 1,
  "source": "fleetward",
  "state": "firing",
  "kind": "verification_failed",
  "severity": "critical",
  "alert_id": "…",
  "fingerprint": "verification_failed:…",
  "instance_id": "…",
  "instance_name": "prod-orders",
  "summary": "A backup of prod-orders failed verification",
  "detail": "…",
  "text": "CRITICAL — A backup of prod-orders failed verification …",
  "fired_at": "2026-09-09T02:00:00Z"
}
```

`text` is the same thing as one sentence, for a receiver that posts straight into a chat channel.
There is no templating: the payload is fixed, and everything in it is already readable through the
API by anyone who can see the alert.

A Slack or Teams incoming webhook is a webhook — point `url` at it. **But note the caveat below.**

### SMTP

```
FLEETWARD_NOTIFIER_SECRET='the-mailbox-password' \
fleetward-cli alert notifier create \
  --name dba-oncall \
  --kind smtp \
  --setting host=smtp.example.com \
  --setting from=fleetward@example.com \
  --setting to=dba@example.com,oncall@example.com \
  --setting username=fleetward \
  --min-severity critical
```

| Setting | Meaning |
|---|---|
| `host` | the server. Required. |
| `port` | defaults to 587, or 465 when `tls` is `implicit`. |
| `from` | the sender. Required. |
| `to` | a comma-separated recipient list. Required. |
| `username` | omit for a relay that needs no authentication. |
| `tls` | `starttls` (the default), `implicit`, or `none`. |

**A username with `tls: none` is refused.** A password must not be sent over an unencrypted
connection, and the standard library agrees — refusing at creation means finding out now rather than
during an outage.

Each message carries `References: <fingerprint@fleetward>`, so a mail client threads a resolution
under the alert it resolves rather than starting a second conversation about the same broken thing.

### The credential

A notifier has exactly one, and it goes to the secrets provider
([ADR-0009](../adr/0009-secrets-provider-interface.md)) rather than into `settings`. It is read from
`FLEETWARD_NOTIFIER_SECRET`, or from stdin with `--secret-stdin`, and never from a flag — a flag is
in your shell history and in the process list.

No read API returns it. `notifier list` says only whether one is stored.

**A `--setting` whose key looks like a credential is refused.** `password`, `token`, `secret`,
`api_key`, `authorization` — because `settings` is returned by `notifier list`, and "just put the
token in settings, it is easier" is exactly the shortcut somebody takes at 2am.

**The caveat that check cannot catch:** a Slack or Teams webhook URL *embeds* its own token, and that
URL lives in `settings.url`, where any administrator listing notifiers can read it. Notifier
management is `admin`-only partly for this reason. If that is not acceptable in your environment,
point Fleetward at a relay of your own and let the relay hold the vendor URL.

### Test it before you rely on it

```
fleetward-cli alert notifier test <notifier-id>
```

A real message goes to the real endpoint, and the outcome is recorded on the notifier's row so
`notifier list` shows it afterwards. There are two moments to discover a destination is
misconfigured: now, or during the incident it was configured for.

## Who may do what

| Action | Role |
|---|---|
| List alerts | `viewer` |
| Acknowledge an alert | `operator` |
| List rules | `viewer` |
| Enable or disable a rule | `operator` |
| Create or delete a rule | `admin` |
| Everything about notifiers | `admin` |

Silencing a noisy rule at 3am is an operator's job, because a person who cannot silence one rule will
mute the whole channel instead. Notifiers are administrator-only: they hold a credential for
somebody else's system, and their settings are readable by whoever can list them.

Every mutating action here lands in `audit_log`, including the ones that were refused. See
[`authorization.md`](authorization.md).

## Configuration

| Setting | Default | What it does |
|---|---|---|
| `FLEETWARD_ALERTS_ENABLED` | `true` | Runs the evaluation pass. False leaves the tables untouched. |
| `FLEETWARD_ALERTS_EVAL_INTERVAL` | `30s` | How often the estate is evaluated. |
| `FLEETWARD_ALERTS_DELIVERY_WORKERS` | `2` | Notifications in flight at once. |
| `FLEETWARD_ALERTS_DELIVERY_QUEUE_SIZE` | `256` | What may be waiting. A full queue drops. |
| `FLEETWARD_ALERTS_DELIVERY_TIMEOUT` | `15s` | One attempt at one destination. |
| `FLEETWARD_ALERTS_DELIVERY_ATTEMPTS` | `3` | Tries before a notification is given up on. |

Thirty seconds is not a tuning choice: it is the staleness the
[roadmap](../roadmap.md#what-real-time-monitoring-means-here) claims, and a default that did not
honour it would make that sentence false.

**Turning evaluation off is loud.** The control plane warns on every start that no condition will
become an alert, because an installation with alerting switched off looks exactly like an estate with
nothing wrong.

## What this does not do

Named here rather than left to be discovered.

- **No durable delivery queue.** See the first section of this page.
- **No alert for an inconclusive verification.** See above, and
  [ADR-0040](../adr/0040-an-inconclusive-verification-is-not-an-alert-about-the-artifact.md).
- **No maintenance windows, no snoozes, no grouping, no inhibition.** Disabling a rule is the whole
  silencing story.
- **No templating.** One JSON body, one plain-text email, both fixed.
- **No vendor-specific notifier kinds.** A Slack incoming webhook is a webhook.
- **No screen.** The API and the CLI are the surface; the web app's Alerts entry is still disabled.
- **No metric for evaluation itself.** "Did the pass run" is answered by the log and by
  `last_success_at` on a notifier. A `/metrics` counter is a later slice.
