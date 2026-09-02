# ADR-0013: Internal cron scheduler with Postgres lease locking

- **Status:** Accepted; what an expired lease does is superseded by [ADR-0025](0025-an-expired-lease-fails-its-job.md)
- **Date:** 2026-07-26

> **Superseded in part, 2026-09-02.** Everything below stands — internal cron, leases in
> PostgreSQL, at-most-once, renewal by heartbeat — except the clause under **Decision** saying that
> "an expired lease from a crashed runner becomes claimable again". Implementing the scheduler in
> slice B1 showed that re-running is the wrong answer for the work this product actually schedules:
> an interrupted backup leaves no artifact to salvage, and starting a six-hour dump against a
> production server unattended, at the moment that server has just come back, is worse than the
> missed run. An expired lease now **fails** its job, with a message saying so, and the next
> scheduled occurrence creates a new one. See
> [ADR-0025](0025-an-expired-lease-fails-its-job.md).

## Context

Fleetward must run backup jobs and verification jobs on schedules. Two properties matter:

1. A job must not run twice concurrently. Two `pg_basebackup` runs against one instance at once is
   an operational incident.
2. Job state must survive a control-plane restart — a backup schedule that silently stops after a
   deploy is worse than no schedule.

## Decision

An internal scheduler in the control plane (`robfig/cron` for cron expression parsing), with:

- Job definitions and run history **persisted in Postgres**, not held in memory.
- **At-most-once execution via lease locking**: a runner claims a job by atomically setting a lease
  with an expiry; leases are renewed while running and released on completion. An expired lease
  from a crashed runner becomes claimable again. *(Superseded: it is failed rather than re-claimed —
  see the note above and [ADR-0025](0025-an-expired-lease-fails-its-job.md).)*

## Consequences

- Correct behaviour under the realistic failure mode — a control-plane restart mid-backup — without
  introducing a distributed queue.
- The same lease mechanism makes a multi-replica control plane possible later with no redesign.
- Postgres is already a hard dependency (ADR-0005), so this adds no new infrastructure.
- We choose **at-most-once** deliberately: a missed backup is recoverable and visible; a duplicate
  concurrent backup can degrade a production instance.
- Cost: lease expiry must exceed the longest plausible job runtime, or a long backup could have its
  lease stolen. Leases are therefore renewed by a heartbeat, not set once.

## Alternatives considered

- **System cron / Kubernetes CronJob.** Pushes scheduling outside the product; no run history, no
  UI, no per-tenant scheduling.
- **A dedicated queue (Temporal, River, NATS).** More capable, but a substantial new dependency for
  what is a modest scheduling need at MVP scale.
- **In-memory scheduling.** Fails requirement 2 outright.
