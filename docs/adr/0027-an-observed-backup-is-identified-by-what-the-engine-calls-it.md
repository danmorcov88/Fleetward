# ADR-0027: An observed backup is identified by what the engine calls it

- **Status:** Accepted
- **Date:** 2026-09-02
- **Slice:** B3 — observed backups
- **Relates to:** [ADR-0015](0015-observed-and-managed-backups.md),
  [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md),
  [ADR-0026](0026-a-shared-directory-carries-a-file-based-artifact.md)

## Context

[ADR-0015](0015-observed-and-managed-backups.md) decided that Fleetward reports on backups it did
not take. It did not decide how one of those is recognised the second time it is seen, and that
question turns out to carry the whole feature.

Observation is a poll. A schedule reads the same source every half hour, for years, and the source
does not change between reads — the backup taken last night is still there this morning and will
still be there next week. Something has to make the two thousandth read of one nightly backup
produce the same row as the first, or Fleetward's own database fills with copies of it: forty-eight
rows a day per backup, an estate that looks a hundred times busier than it is, and a retention
question in B5 that has to guess which copy is the real one.

The identity therefore cannot come from the moment of observation. It has to come from the source.

The two sources this slice implements sit at opposite ends of what a source can offer, which is why
B2 was ordered before B3:

| Source | What it can offer | Survives |
|---|---|---|
| An engine's own backup history — SQL Server's `msdb.dbo.backupset` | `backup_set_uuid`, assigned by the engine when the backup set is written | anything short of deleting the row |
| A directory of files, which is all PostgreSQL leaves behind | a name, a size, a modification time | not a rename, not a move |

There is a second problem hiding behind the first, and it was found by reading `msdb` rather than by
reasoning about it. **An engine that keeps its own backup history records Fleetward's backups
too.** A managed backup Fleetward takes on SQL Server writes a row into `backupset` exactly as
everyone else's does, so the very next observation poll sees the backup Fleetward itself took and
has no way to know it. One physical backup, two rows, one of them claiming an origin it does not
have — and the managed row is the one carrying the manifest, so a screen showing both would offer a
verification on one and not the other for the same backup.

## Decision

**The plugin supplies the identity, core never interprets it, and a managed backup records the one
the engine gave it.**

Three parts, all additive to the contract.

```proto
message ObservedBackup {
  // Stable across polls and unique within the instance, as the engine names it rather than as this
  // poll happened to see it.
  string external_id = 1;
}

message BackupHistoryCapabilities {
  // The identity is assigned by the engine, and therefore survives the artifact being moved.
  bool identity_is_engine_assigned = 3;
}

message BackupResult {
  // What the engine called the backup this call just took.
  string external_id = 12;
}
```

In the schema it is one partial unique index, and the upsert that uses it:

```sql
CREATE UNIQUE INDEX idx_backups_external_identity
    ON backups (instance_id, external_id)
    WHERE external_id IS NOT NULL;
```

```sql
INSERT INTO backups (..., origin, external_id, ...) VALUES (..., 'observed', $4, ...)
ON CONFLICT (instance_id, external_id) WHERE external_id IS NOT NULL
DO UPDATE SET ...
WHERE backups.origin = 'observed'
```

Three properties follow, and each is one of the problems above:

- **A poll is idempotent.** The same evidence read again updates one row.
- **Re-reading is free, so the watermark can be generous.** Core derives where to resume from
  `max(completed_at)` over what it already recorded, then deliberately reads back further than that
  — six hours — because evidence does not always arrive in the order it was created. The overlap
  costs one wasted comparison per record; being mean with it costs a backup nobody ever sees.
- **The two origins converge rather than collide.** The `WHERE backups.origin = 'observed'` on the
  update is what protects a managed backup: the conflict is resolved, no duplicate is inserted, and
  the managed row — with its manifest, its checksum, and its artifact — is left exactly as it was.

**A plugin whose source assigns no identity derives one, and declares that it did.** The PostgreSQL
directory source digests the file's name and nothing else — not its size, not its modification time
— so a dump written to the same path every night is one record whose finish time moves, and a file
still being written while a poll runs updates one record rather than inserting a second on the next
poll. What that cannot survive is a rename, and `identity_is_engine_assigned = false` is how
Fleetward says so in its own reporting instead of double counting silently.

## Consequences

**Core carries an opaque string it must never parse.** `external_id` is `backup_set_uuid` for one
engine and `file:<digest>` for another, and it will be something else again for Oracle. Core upserts
on it, compares it for equality, and does nothing else with it — which is the same discipline
`method_id` and `metadata` already live under.

**A plugin that returns an empty identity fails the poll loudly.** Silently skipping the record
would report a gap in somebody's backups that is really a gap in ours, so it is a typed error naming
the contract violation. The conformance suite asserts a non-empty identity, and asserts that reading
the same evidence twice yields it exactly once.

**A renamed artifact appears twice on a weak source, and this is not fixed here.** It is reported —
the caveat reaches the adherence answer and the history listing — rather than papered over with
matching heuristics core would have to invent. Fuzzy matching on time and size is exactly the
engine-shaped guessing the capability matrix exists to replace.

**The watermark is derived, not stored.** A column holding "where we got to" would be a second source
of truth that can disagree with the rows it describes: lost in a restore of the metadata database,
stale after a poll that half succeeded, wrong forever if anything ever wrote it out of order.
Reading it back off the rows cannot drift and self-heals after a missed poll. It costs one indexed
`max()` per poll.

**An instance whose first poll happens years after its backups started is bounded rather than
complete.** The first poll of an instance with no watermark reads a thirty-day horizon, because
asking an engine that has been up for six years for its entire backup history is the query this
whole design exists to avoid running against a production server. Thirty days answers every
adherence window anybody declares; deeper history is not lost, it is simply not imported.

## Alternatives considered

**Core derives the identity from fields it already understands** — instance, database, start time,
method. Needs no contract field at all, which is genuinely attractive. It fails on an engine that
records two backups within the same second, and it re-inserts every row the moment an engine
corrects a timestamp. Worse, it makes core's notion of identity a thing plugin authors have to
reverse-engineer rather than a thing they declare.

**A hash of the whole record.** Trivially stable and trivially wrong: any change at all produces a
new identity, so a backup observed while running and then observed as finished becomes two rows —
the exact duplication being prevented, arriving through the mechanism meant to prevent it.

**Accept that Fleetward's own backups appear twice.** Adherence would still answer correctly, since
a window is satisfied by any backup, so the cost is not in the data — it is entirely in the human. A
history list showing every managed backup twice is a list a DBA stops trusting, and a tool whose
central claim is that it tells you the truth about your backups cannot afford to be visibly wrong
about how many there are.

**Core matches observed evidence against managed rows** on time, database and size, and suppresses
what looks like a duplicate. It would work most of the time, which is the problem: the failure mode
is silently hiding a real backup that happened to resemble one of ours, on the one screen that
exists to say what really happened.

**A separate `observed_backups` table**, avoiding any interaction with `backups` at all. Clean in
isolation. Rejected because adherence asks one question — did a backup happen inside this window —
and it does not care who took it, so every such query becomes a `UNION` and every future query
becomes an opportunity to forget one half. ADR-0015 says the distinction runs through the schema; a
column is that distinction, and a second table is a second schema.
