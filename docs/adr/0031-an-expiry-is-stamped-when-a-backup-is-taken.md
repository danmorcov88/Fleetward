# ADR-0031: A backup's expiry is stamped when it is taken, not computed from its schedule

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B5 — retention and expiry
- **Relates to:** [ADR-0028](0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md),
  [ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
  [ADR-0032](0032-retention-never-deletes-the-last-good-backup.md)

## Context

`schedules.retention_days` has existed since the first migration, with a default of 30 and a
`CHECK (retention_days > 0)`. So has `backups.expires_at`. The first was written on every schedule
ever created and read by nothing; the second was never written at all.

Retention needs one number per backup: the instant after which its artifact may be destroyed. There
are two places that number can come from, and four cases sit between them where they disagree:

- a backup whose schedule was deleted, so `backups.schedule_id` is now NULL (`ON DELETE SET NULL`);
- a backup a human triggered, which never had a schedule at all;
- an instance with two enabled backup schedules carrying different retention;
- a schedule whose `retention_days` is edited after backups were taken under the old value.

Slice B3 faced a structurally similar choice and went the other way. `ADR-0028` made schedule
adherence **computed on read**: nothing is stored, so nothing can go stale, and an edited
declaration immediately changes the verdict. The question was whether that reasoning transfers.

## Decision

**`expires_at` is written once, when the backup succeeds, from the `retention_days` the schedule
carried at the moment the job was materialized. It is never recomputed. A run with no retention
behind it is stamped NULL, and NULL means the artifact is never deleted.**

The value travels the same road `method_id`, `options` and `verify_policy` already travel: snapshot
into `jobs.payload` when the schedule is materialized, then through `BackupJob` and
`RunBackupInput` to `recordSuccess`. A schedule edited or deleted while a backup is running changes
nothing about that run, exactly as it already changes nothing about which method it uses.

The four cases resolve without a special rule for any of them:

| Case | What happens |
|---|---|
| the schedule was deleted | nothing — the expiry was written before the schedule went |
| a manual backup | NULL. Nothing declared a retention, and Fleetward does not invent one in order to delete something |
| two schedules, different retention | each backup carries the retention of the schedule that made it |
| `retention_days` edited | old backups keep the old expiry; the edit applies from the next backup onward |

### Why ADR-0028's reasoning does not transfer

Adherence computes a **report**. It must reflect the declaration in force *now*, and it is harmless
when it changes — the worst outcome of recomputing is that a screen says something different today
than yesterday, which is what a screen is for.

Retention computes an **authorisation to destroy**. It must reflect the declaration in force when
the backup was taken; it must be stable, because a value that changes underneath a deletion is a
deletion nobody authorised; and it must be auditable after the fact, which a value that exists only
as the output of a query never is.

A report may change its mind. A deletion may not.

### The property that decides the slice's blast radius

**Stamping means the migration backfills nothing.**

Every backup taken before this slice has `expires_at IS NULL`. NULL is never selected by the sweep,
so the first sweep after an upgrade deletes exactly zero objects, and the estate begins expiring
only backups taken after the operator upgraded — at the earliest, a full retention period later.
That is a whole retention period of warning, during which `fleetward-cli backup retention` fills up
and can be read.

Computing from the schedule has the opposite property, and it is not a detail: it is retroactive by
construction. The first tick after `docker compose up` would have deleted a year of history from an
estate whose owner had asked for nothing.

## Consequences

**Lowering a schedule's retention does not shrink what is already stored.** Some operators will
expect it to. The alternative is that editing a number silently destroys fifty artifacts on the next
tick, which is the surprise this slice exists not to deliver. The deliberate path to the other
behaviour is a human re-stamping existing backups on purpose, with its own confirmation surface;
that is **not built here** and should not be added as a side effect of anything.

**Deleting a schedule no longer has a hidden consequence.** `ON DELETE SET NULL` on
`backups.schedule_id` would, under a computed design, silently move every one of that schedule's
backups from its retention to whatever the fallback was. Under this design it moves them to nothing
at all, because the value was already written.

**A manual backup is kept forever unless somebody removes it.** That costs storage, and it is the
safe direction: a human asked for that artifact specifically, and no policy attached to it says when
it stops being wanted.

**`retention_days` is now load-bearing on a schedule and is read exactly once per run.** A schedule
of kind `observe` or `discovery` carries the column too and it means nothing there, because those
kinds produce no artifact.

## Alternatives considered

**Compute the expiry on read from the schedule, as B3 does for adherence.** The consistent choice,
and it needs no new write path. Rejected for the four disagreements above and, decisively, for what
it does on the first tick after an upgrade. The consistency is superficial: adherence and retention
look alike and are not, because only one of them ends in something being destroyed.

**Compute on read, with a global default for backups whose schedule is gone.** Fixes the NULL case
by inventing a policy for a backup nobody wrote a policy for, and leaves the retroactive-edit and
first-sweep problems untouched. It also makes deleting a schedule a destructive act with no warning,
which is a thing nobody expects a delete-a-schedule button to be.

**Stamp at backup *start* rather than at success.** Simpler plumbing — `createRows` already receives
the input. Rejected because a backup legitimately runs for hours, and "keep this for thirty days"
means thirty days from when the artifact existed, not from when the dump began. It would also stamp
expiries onto rows that go on to fail, which the sweep ignores but which read as nonsense to a human.

**Store the retention on the backup row as a number of days rather than as an instant.** Equivalent
in every case that matters and worse in one: an instant is directly indexable, and
`idx_backups_expiring ON backups (expires_at) WHERE state = 'succeeded'` has been waiting in the
schema since migration 000001 for exactly this column to be written.
