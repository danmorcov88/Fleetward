# Retention and expiry

How Fleetward decides that a backup artifact has outlived its usefulness, what it refuses to delete
whatever any policy says, and how to see the answer before it acts on it.

Every setting named here is documented in [`configuration.md`](configuration.md). The decisions
behind the design are [ADR-0030](../adr/0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
[ADR-0031](../adr/0031-an-expiry-is-stamped-when-a-backup-is-taken.md) and
[ADR-0032](../adr/0032-retention-never-deletes-the-last-good-backup.md).

---

## The one rule with no exceptions

**Fleetward never deletes a backup it did not take.**

An `observed` backup is somebody else's file — written by a cron job, a script, or tooling that
predates Fleetward ([ADR-0015](../adr/0015-observed-and-managed-backups.md)). Fleetward reports on
it and owns nothing about it. There is no setting that changes this, and it is not enforced by a
filter in a query that somebody could later forget: the database refuses the state transition
outright, so a query written next year by an author who has never read that decision fails loudly
rather than destroying a DBA's own `pg_dump` output.

Only a `managed` backup — one Fleetward ran, whose artifact it wrote to its own object storage — is
ever a candidate.

## An expiry is stamped when the backup is taken

A schedule declares how long its backups are kept:

```bash
fleetward-cli schedule create --instance prod-1 --cron "0 2 * * *" --retention-days 14
```

When a backup taken under that schedule succeeds, `14 days from now` is written onto the backup row
and never recalculated. Three consequences follow, and all three are the point:

- **Deleting the schedule changes nothing.** The value was written before the schedule went.
- **Editing `--retention-days` applies from the next backup onward**, not retroactively. An edit to
  a number does not destroy fifty artifacts on the next tick.
- **A backup with no expiry is never deleted.** That is every manual `backup run`, and every backup
  taken before this version of Fleetward existed.

That last point is what makes an upgrade safe. **Upgrading to a version with retention deletes
nothing.** No backup that already exists carries an expiry, so the estate begins expiring only
backups taken after the upgrade — at the earliest, a full retention period later.

## What is never deleted, however old it is

Retention that is purely time-based will, on a server whose backups have been failing for a month,
delete the last backup that worked. That is the ordinary result of a correct implementation, and it
is the most damaging thing this product could do. So there is a floor, and it keeps two things per
instance:

| Kept | Why |
|---|---|
| the most recent **successful** backup | an instance must never be left with nothing to restore from |
| the most recent backup **proven restorable** | on an instance failing verification, the newest backup is known to be *bad*; this is the last one known to be good |

Often the same row, in which case the floor costs one artifact per instance.

`FLEETWARD_RETENTION_MIN_KEEP` widens the first rule to N. **It cannot be set to zero** — the control
plane refuses to start — because a floor that can be configured away is not a floor.

Two things that are deliberately *not* how verification participates:

- **An unverified backup is not deleted sooner.** "Unverified" nearly always means nobody checked —
  a `sampled` policy, a busy verification queue, a container runtime that went away — and deleting
  on the basis of ignorance would make an unhealthy estate lose its backups faster.
- **A failed verification does not delete sooner either.** The artifact is the evidence behind the
  loudest alert Fleetward raises, and an operator is about to want to look at it.

## Seeing it before it happens

```
fleetward-cli backup retention
retention runs every 1h0m0s, keeps at least 1 recent backup(s) per instance, and deletes at most 500 artifacts per sweep

WOULD BE DELETED (3, 4.1 GiB)
INSTANCE  BACKUP    FINISHED (UTC)       EXPIRED (UTC)        SIZE
prod-1    9f2c…3a1  2026-07-20 02:04:11  2026-08-19 02:04:11  1.4 GiB
prod-1    a71e…88d  2026-07-21 02:03:55  2026-08-20 02:03:55  1.4 GiB
prod-2    c40b…19f  2026-07-21 02:11:02  2026-08-20 02:11:02  1.3 GiB

PAST ITS RETENTION AND KEPT ANYWAY (2)
INSTANCE  BACKUP    FINISHED (UTC)       EXPIRED (UTC)        SIZE     WHY IT STAYS
prod-3    2d81…7c4  2026-06-02 02:02:47  2026-07-02 02:02:47  980 MiB  kept: it is the most recent backup of this instance proven restorable, and deleting the last proof is worse than keeping one old artifact
prod-3    ee09…b52  2026-08-30 02:01:19  2026-09-02 02:01:19  1.1 GiB  kept: it is this instance's most recent successful backup, which retention never deletes however old it is
```

Nothing is deleted by running it, and it answers whether or not the sweep is enabled — the person
deciding *whether* to enable retention is exactly the reader it is for. When the sweep is off it
says so on the first line, because listing artifacts as "would be deleted" without mentioning that
nothing is running would be misleading in the one place this product cannot afford to be.

`--instance <name>` narrows it to one server.

## How a sweep runs

Retention is **estate-wide**, not per instance. It is not a schedule and there is no `retention` job
kind: the sweep runs on the control plane's own tick, beside the reaper that closes abandoned jobs,
every `FLEETWARD_RETENTION_INTERVAL` (one hour by default).

It holds no lease, and that is deliberate rather than an oversight. Two control planes sweeping at
the same moment is not a race to lose, because the work is idempotent: the state change is its own
guard, so a given backup is expired by exactly one of them and the other matches nothing, and
deleting an object that is already gone is not an error. This is also what lets a healthy replica
finish work an unhealthy one abandoned.

One consequence worth knowing: **there is no job row per sweep**, so `fleetward-cli job list` will
never show one. What records a sweep is the rows it touched, plus a log line:

```
INFO  retention sweep finished  expired=3 artifacts_deleted=3 bytes_reclaimed=4402341888
```

A sweep that did nothing says nothing.

## What "deleted" means

A backup is a row in the metadata database and an object in storage, and there is no transaction
across the two. The order is fixed:

1. the row moves from `succeeded` to `expired`, and that commits on its own. From that instant it no
   longer claims a restorable artifact — a verification against it is refused, and the estate view
   reads honestly;
2. the object is deleted;
3. **the row is never deleted.** `bucket` and `object_key` stay exactly as they were, so an audit
   can still show what once existed and where.

A control plane killed between steps 1 and 2 leaves a backup marked `expired` whose bytes are still
in the bucket. That is expected and it repairs itself: the next sweep — on any control plane, not
necessarily the one that died — finishes the deletion. Until it does, the row appears under
*ALREADY EXPIRED, ARTIFACT STILL PRESENT* in `backup retention`.

## Nothing is deleted while it is being read

A verification downloads the artifact when it runs, and a restore does the same. A backup with a
verification or a restore in progress — or with one merely queued, which is a job before it is
anything else — is not eligible, and becomes eligible again as soon as that finishes. This is a
delay, not a reprieve.

## Settings

| Setting | Default | What it does |
|---|---|---|
| `FLEETWARD_RETENTION_ENABLED` | `true` | Whether the sweep runs at all. `false` leaves every artifact in place, which is what every version before this one did. |
| `FLEETWARD_RETENTION_INTERVAL` | `1h` | How often the sweep runs. Far longer than the scheduler's poll interval on purpose. |
| `FLEETWARD_RETENTION_MIN_KEEP` | `1` | How many recent successful backups of an instance are kept whatever their expiry says. **Zero is refused at startup.** |
| `FLEETWARD_RETENTION_MAX_PER_SWEEP` | `500` | How many artifacts one sweep may delete. A ceiling so that a bug is bounded, not because the query is slow; a backlog drains over successive sweeps. |

## Turning it off

`FLEETWARD_RETENTION_ENABLED=false` is a legitimate configuration and is not reported as unhealthy.
An operator who wants to watch `backup retention` for a few weeks before trusting it should use it —
that is what it is for.
