# ADR-0030: Retention sweeps the estate on the tick, and deletes the artifact behind a row it never removes

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B5 — retention and expiry
- **Relates to:** [ADR-0013](0013-internal-scheduler-with-leases.md),
  [ADR-0015](0015-observed-and-managed-backups.md),
  [ADR-0025](0025-an-expired-lease-fails-its-job.md),
  [ADR-0031](0031-an-expiry-is-stamped-when-a-backup-is-taken.md),
  [ADR-0032](0032-retention-never-deletes-the-last-good-backup.md)

## Context

Every slice before this one read, reported, or added. This one removes, and that changes what a bug
costs: until now the worst outcome was a wrong answer on a screen, and from here it is a backup that
is gone.

Two mechanical questions had to be settled before a line was written.

**What kind of work is a sweep?** Every recurring thing Fleetward does is a `schedules` row that
materializes into a leased `jobs` row ([ADR-0013](0013-internal-scheduler-with-leases.md)), and B3
and B4 each added one of those. Retention does not fit the mould: `retention_days` is stored per
schedule, but *the sweep* is estate-wide. It answers "what in this bucket has outlived its
retention", not "what should happen to this instance".

**In what order does a backup stop existing?** A backup is a row in PostgreSQL and an object in S3.
There is no transaction across the two, so one of them commits first, and whichever is second owns
the crash window.

## Decision

### 1. The sweep runs on the scheduler's tick, beside the reaper, and holds no lease

`Runner` gains a `SweepRetention` method, which is the only thing on that interface that is not a
job. The scheduler calls it from `tick`, paced by `FLEETWARD_RETENTION_INTERVAL` rather than by the
poll interval, in a goroutine that joins the same wait group as every running job so `Close` waits
for it.

**No lease, because retention is idempotent in a way a backup is not.** The state transition is its
own guard: `UPDATE backups SET state = 'expired' WHERE state = 'succeeded' AND …` matches a given
row for exactly one of two concurrent sweeps, and the other sees nothing. Object deletion is
idempotent by the object store's own contract — "deleting an object that does not exist is not an
error". So the lease would be protecting against a collision that cannot occur, which is precisely
why it is required for a `pg_dump` and not here.

### 2. The row is expired first, the object is deleted second, and the row is never deleted

Three steps:

1. `succeeded → expired`, committed on its own. From that instant the row no longer claims a
   restorable artifact: `loadVerificationTarget` refuses anything that is not `succeeded`, and the
   estate view reads honestly.
2. The object is deleted. Only then is `backups.artifact_deleted_at` written.
3. There is no third step. Retention never issues `DELETE FROM backups`, and `bucket` and
   `object_key` are left intact — the schema has said since migration 000001 that they are "kept
   even after expiry so an audit can show what once existed".

A control plane killed between 1 and 2 leaves a row that is `expired` with `artifact_deleted_at IS
NULL` and an object still in the bucket. **That leftover is self-reconciling**: step 2 selects on
exactly that predicate, so the next sweep — on any control plane, not necessarily the one that died
— finishes it, and the re-run is free because deleting an absent object is not an error.

### 3. An observed backup cannot be expired, and it is the database that says so

A `CHECK (NOT (origin = 'observed' AND state = 'expired'))` on `backups`.

[ADR-0015](0015-observed-and-managed-backups.md) says Fleetward must never delete a backup it did
not take. The obvious implementation of that promise is `WHERE origin = 'managed'` in the retention
query, and the obvious implementation is not good enough: it is a line somebody deletes while
refactoring, or forgets to repeat in a second query written by an author who has not read that
record. The consequence of forgetting it is destroying a customer's own backups, which is the one
mistake this product cannot make.

So retention's only means of action — setting `state = 'expired'` — is made illegal on an observed
row. A query that forgets the filter raises `23514` and rolls back, loudly, instead of succeeding
quietly. That is three independent barriers with the strongest at the bottom: the query filters, an
observed row's `object_key` is empty by construction, and the database refuses.

## Consequences

**There is no `jobs` row per sweep, so `job list` cannot answer "did retention run last night".**
This is the real cost of decision 1, and it is paid deliberately. What replaces the job row is the
`expired` state and `artifact_deleted_at` on the rows themselves, a log line per non-empty sweep,
and `GET /api/v1/backup-retention` — the preview, which reads through the *same* SQL the sweep does,
so an operator can see the answer before it is acted on rather than reconstruct it afterwards. A
preview that answered a slightly different question would be worse than none, because it would be
believed.

**Two control planes both sweep, and that is correct rather than tolerated.** It is also what makes
a leftover from a crashed replica get cleaned up by a healthy one, the same property that made
reaping-on-the-tick right in [ADR-0025](0025-an-expired-lease-fails-its-job.md).

**A sweep is bounded by `FLEETWARD_RETENTION_MAX_PER_SWEEP`, default 500.** Not because the query is
slow — `idx_backups_expiring` has existed since migration 000001 — but because a destructive loop
with no ceiling is the wrong shape however correct it looks. A backlog drains over successive
sweeps.

**The first tick after a start sweeps.** That is deliberate: it is how a leftover from a crash is
finished promptly rather than an hour later.

**`expired` is now unavailable as a word for "the DBA's own retention removed this file".** An
observed backup whose file has gone is a different fact — evidence read from an engine, not an
action Fleetward took — and being forced to give it a different word if that is ever wanted is the
point rather than the cost.

**Deleting an instance still orphans its objects.** `backups.instance_id` is `ON DELETE CASCADE`, so
the rows go and the bytes stay, and `DeleteInstance(delete_artifacts=true)` remains declared and
unimplemented. This slice does not close that; it does make it the only remaining way to orphan an
object, and object keys are `tenants/<t>/instances/<i>/backups/<b>/artifact`, so a
bucket-versus-rows reconciliation is straightforward when it is wanted.

## Alternatives considered

**A `retention` schedule kind, one per instance.** The tidy answer, consistent with `backup`,
`observe` and `discovery`, and it would have reused the lease machinery unchanged. Rejected because
it puts a per-instance schedule in front of an estate-wide property: fifty schedules to create by
hand, and — far worse — an instance whose retention schedule nobody created grows forever *in
silence*. The failure mode of that design is the exact failure this slice exists to remove.

**A job kind with no `schedules` row behind it.** Would have kept the lease and given a job row per
sweep, which is the operational surface decision 1 gives up. Rejected on price: `jobs.instance_id`
is `NOT NULL` and `idx_jobs_one_active_per_instance_kind` is keyed on it, so an estate-wide job does
not fit the table without making that column nullable — weakening the constraint whose entire
purpose is to stop two backups running against one production server. Paying that in order to
schedule a `DELETE` is a bad trade.

**Delete the object first, then update the row.** Rejected: a crash in between leaves a row saying
`succeeded` and pointing at an artifact that is gone. The estate view and `backup show` would render
a backup that does not exist, and a verification against it would fail with a confusing cause. It is
the one leftover that is actively misleading rather than merely untidy.

**Hard-delete the row, then the object.** Rejected twice over. A crash in between leaves an object
nothing points at — invisible, and findable only by listing the bucket. And it does not work anyway:
`restores.backup_id` is `ON DELETE RESTRICT`, so the database already refuses to remove the row of
any backup that was ever restored.

**Protect observed backups with a `WHERE` clause alone.** Rejected as the load-bearing barrier, and
kept as one of three. A predicate is correct on the day it is written and is not a guarantee about
the query somebody writes next year.
