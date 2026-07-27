# ADR-0013: Internal cron scheduler with Postgres lease locking

- **Status:** Accepted
- **Date:** 2026-07-26

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
  from a crashed runner becomes claimable again.

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
