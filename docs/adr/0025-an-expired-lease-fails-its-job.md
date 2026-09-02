# ADR-0025: An expired lease fails its job rather than making it claimable again

- **Status:** Accepted
- **Date:** 2026-09-02
- **Slice:** B1 — the scheduler and the job lease
- **Supersedes:** the "becomes claimable again" clause of [ADR-0013](0013-internal-scheduler-with-leases.md);
  everything else in that record — internal cron, Postgres leases, at-most-once, heartbeat renewal —
  is unchanged and is what this decision rests on
- **Relates to:** [ADR-0015](0015-observed-and-managed-backups.md),
  [ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md),
  [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md)

## Context

[ADR-0013](0013-internal-scheduler-with-leases.md) settled that jobs are leased, that leases are
renewed by a heartbeat, and that at-most-once is the property worth buying. It also said, in one
clause, what should happen to a lease that expires:

> An expired lease from a crashed runner becomes claimable again.

The schema repeated it, in the comment on the `jobs` table's lease columns. Neither had been
implemented, so neither had met the case it describes.

Implementing it, the case turned out to be narrower and more expensive than the clause suggests. A
lease expires for exactly one realistic reason: the process holding it stopped — `kill -9`, an OOM
kill, a node that lost power, a deploy that did not drain. What that process leaves behind is
specific:

- **No artifact.** A backup writes through presigned multipart part grants, and core completes the
  upload only after the plugin reports success ([ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md)).
  A run interrupted half way leaves parts that no object ever references. There is nothing to
  salvage and nothing to resume.
- **A `backups` row saying `running`** that describes an artifact which does not exist. This is the
  oldest known defect in the tree: a row stuck at `running` for weeks, in a product whose entire
  purpose is to tell a DBA whether their backups are healthy.
- **A `verifications` row saying `running`**, possibly with a sandbox container that the startup
  orphan sweep will remove.

Making the job claimable again means the next tick starts the whole run over. For a backup that is
a `pg_dump` against a production server, potentially of a large database, potentially at 09:00 on a
Monday rather than in the maintenance window it was scheduled for — and, in the crash-loop case,
once per restart. The moment a control plane recovers from an incident is the worst possible moment
to start unattended heavy work against every instance it watches.

Against that, what re-running buys is one earlier backup. The schedule that created the job will
create another one at its next occurrence, and that occurrence is when its owner asked for it.

There is a second, quieter argument. If a `running` job with an expired lease can be re-claimed,
then a stalled process that comes back — a paused VM, a long stop-the-world pause, a host that lost
its network for three minutes — can be mid-`pg_dump` while a second runner starts another one
against the same instance. The lease's heartbeat is designed to detect that and cancel, but it
detects it *after* the fact, within one heartbeat interval. Never re-claiming removes the window
instead of narrowing it.

## Decision

**A job whose lease expires is failed, with a message saying so. It is never claimed a second
time.**

Concretely, on every tick a reaper runs one transaction:

1. Jobs in state `running` whose `lease_expires_at` has passed become `failed`, with
   `error_message` explaining that the runner holding the lease stopped reporting, that the job was
   closed rather than re-run, and that the next scheduled run will proceed normally.
2. Every `backups` row those jobs orphaned, still `pending` or `running`, becomes `failed` with the
   same message.
3. Every `verifications` row they orphaned becomes **`inconclusive`**, never `failed`. A control
   plane that was killed is evidence about the control plane, and `FAILED` is reserved for evidence
   about the artifact ([ADR-0022](0022-failed-and-inconclusive-are-different-answers.md)).

The claim query therefore selects only `state = 'pending'`. A job row moves `pending → running`
exactly once, ever.

This supersedes the "becomes claimable again" clause of ADR-0013. Everything else in that record —
internal cron, leases in PostgreSQL, at-most-once, heartbeat renewal — stands unchanged, and
ADR-0013 carries a reciprocal note pointing here.

The heartbeat's cancellation is unaffected and still necessary. Its job is no longer to prevent a
second run, which the database now prevents outright; it is to stop orphaned work — a `pg_dump`
nobody is waiting for, a sandbox nobody will tear down, a connection to a production server held by
a process whose bookkeeping has been taken away.

## Consequences

- **The oldest debt in the tree closes.** A backup interrupted by `kill -9` becomes visible as
  failed, with a reason, within one lease TTL of the crash, instead of reading `running` forever.
- **Recovery is boring.** A control plane that comes back does not begin heavy work; it closes the
  books and waits for the next scheduled occurrence.
- **A crash costs one run.** That is the price, stated plainly. A missed backup is recoverable and
  visible; an unplanned concurrent one against a production instance is an incident.
- **Retry policy becomes a separate, later decision.** `jobs.max_attempts` exists and is untouched
  by this slice. If retries are wanted, they should be a deliberate policy with backoff and a window
  — "the runner died, so start over immediately" is not that policy wearing a different name.
- **The schema comment is now wrong** and is corrected in place, since it describes behaviour rather
  than recording a decision.
- **Observed backups are unaffected.** This concerns only runs Fleetward manages
  ([ADR-0015](0015-observed-and-managed-backups.md)).

## Alternatives considered

**Re-claim the job, as ADR-0013 originally said.** The design this record changes. It is the right
answer for short, idempotent, cheap work — and a backup is none of those. It also reintroduces the
window in which a stalled process and its replacement both run against one instance.

**Re-claim only if the job has attempts left.** A softer version, using `max_attempts`, which the
schema already carries. Rejected because it makes the dangerous behaviour the default and bounds it
by a counter, rather than deciding whether the behaviour is wanted at all. If retries are added
later they should be an explicit policy with backoff, not a side effect of a lease expiring.

**Leave the job `running` and let a human decide.** Honest, and it does surface the problem — but it
leaves the instance blocked: `idx_jobs_one_active_per_instance_kind` counts `running` jobs, so every
subsequent scheduled backup for that instance would be skipped until someone intervened. A crash on
a Friday would mean no backups until Monday, which is the failure this product exists to prevent.

**Reap on startup only, rather than on every tick.** Simpler, and it covers the single-replica
restart case, which is the common one. Rejected because it covers nothing else: a replica that is
partitioned from the database but still running, or one that hangs without exiting, would hold its
jobs indefinitely with no other process able to close them. Reaping on the tick makes any healthy
replica able to clean up after any unhealthy one, which is also what makes multi-replica operation
possible without a redesign.

**Distinguish resumable work from non-resumable work and re-claim only the former.** Attractive, and
probably right eventually — a `discovery` probe is cheap and idempotent in a way a backup is not.
Rejected for this slice because the only kinds the scheduler runs today are `backup` and `verify`,
and both are expensive. Introducing the distinction now would mean designing a general policy from
one example.
