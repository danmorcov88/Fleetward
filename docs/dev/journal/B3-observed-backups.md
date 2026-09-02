# B3 — Observed backups, and the first answer Fleetward can give on day one

- **Delivered:** 2026-09-02
- **Brief:** [B3-observed-backups.md](../slices/B3-observed-backups.md)

Until this slice Fleetward could report only on backups it took itself, which meant it had nothing
to say about the estate it exists for. [ADR-0015](../../adr/0015-observed-and-managed-backups.md)
had already decided why that was wrong — a tool that demands you migrate fifty production servers'
backup arrangements before showing you anything useful does not get adopted — and B3 is that
decision built.

**The deliverable is one sentence: point Fleetward at an estate, change nothing on it, and get the
answer to "did every server's backup run on schedule, and did it succeed".** It cost the contract one
RPC and six fields, all additive, and a second migration — the first since the schema was written.

## How it was verified

On Windows (amd64), 2026-09-02, against Go 1.25.6, Docker 27.3.1, `postgres:16-alpine`, and
`mcr.microsoft.com/mssql/server:2022-latest`.

**`go test -tags=conformance -timeout 60m ./test/conformance/...` passes in 167 seconds**, with the
new case running for both real engines:

```
--- PASS: TestBackupHistoryIsObservable/postgresql          (3.34s)
--- PASS: TestBackupHistoryIsObservable/sqlserver          (15.04s)
--- PASS: TestBackupHistoryIsRefusedWhenNotDeclared/mongodb (0.00s)
--- PASS: TestBackupHistoryIsRefusedWhenNotDeclared/mysql   (0.03s)
--- PASS: TestBackupHistoryIsRefusedWhenNotDeclared/redis   (0.00s)
```

Both halves of the capability are gated, which is the point: a plugin that declares it can see
backup history is held to producing it, and a plugin that declares it cannot is held to *refusing*
rather than answering with an empty list. The three that only handshake exercise the second.

**The PostgreSQL case runs on this machine, and the backup cases still do not.** That is not a
coincidence and it is worth writing down: observation shells out to nothing. `required_tools` gates
the backup and restore path, so the history case deliberately does not consult it — a plugin that
cannot back this instance up on this host can still answer perfectly well for the backups somebody
else took, which is the whole premise of the slice.

**The integration suite covers core's half against a real PostgreSQL**, including the three
properties that would otherwise only be arguments:

```
--- PASS: TestObservationIsIdempotent                          (4.4s)
--- PASS: TestObservationConvergesWithAManagedBackup           (4.2s)
--- PASS: TestObservationRecordsWhatTheEvidenceCanProve        (4.3s)
--- PASS: TestAnObservedBackupCannotBeVerified                 (4.2s)
--- PASS: TestObservationIsRefusedWhenThePluginCannotSeeAny     (4.4s)
--- PASS: TestAdherenceAnswersTheQuestionTheProductExistsFor    (4.1s)
--- PASS: TestMigrationsRunBothWays                            (4.6s)
```

`golangci-lint run` reports no issues outside the known `core.autocrlf` noise, `buf lint`,
`buf format --diff` and `buf breaking --against main` are clean, `go mod tidy` leaves no drift, and
`go run ./tools/docscheck` passes over 64 files.

## The decision the slice existed to make

**What identifies an observed backup**, recorded as
[ADR-0027](../../adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md).

Observation is a poll. The same source is read every half hour for years and does not change between
reads, so something has to make the two thousandth read of one nightly backup produce the same row
as the first. That identity cannot come from the moment of observation; it has to come from the
source, and the plugin is the only thing that knows what its source can offer.

So `ObservedBackup.external_id` is opaque to core — `backup_set_uuid` for one engine, a digest of a
file name for another — and the schema turns it into a partial unique index and an upsert. Three
things fall out of that and each is worth more than the field cost:

- A poll is idempotent, so a nightly backup is one row rather than forty-eight a day.
- **Re-reading is free, so the watermark can be generous.** Core derives where to resume from
  `max(completed_at)` over what it already recorded and then deliberately reads six hours further
  back, because evidence does not always arrive in the order it was created.
- **The watermark is derived rather than stored**, so it cannot go stale, cannot be lost in a restore
  of the metadata database, and self-heals after a missed poll. It costs one indexed `max()`.

### The thing found by reading `msdb` rather than by reasoning

**An engine that keeps its own backup history records Fleetward's backups too.** A managed SQL Server
backup writes a row into `msdb.dbo.backupset` exactly as everybody else's does, so the next
observation poll sees the backup Fleetward itself took and has no way to know it. One physical
backup, two rows, one claiming an origin it does not have — and the managed row is the one carrying
the manifest, so a screen showing both would offer a verification on one and not the other for the
same backup.

The fix is one more additive field, `BackupResult.external_id`: the plugin reports what the engine
called the backup it just took, and the observation poll's upsert lands on the row that already
exists. The `WHERE backups.origin = 'observed'` on the `DO UPDATE` is what makes it safe — the
conflict resolves, nothing is inserted, and the managed row is left exactly as it was. The two
origins converge rather than needing to be de-duplicated, so there is never a moment where two rows
exist and something has to choose.

## What PostgreSQL can actually observe, and what it cannot

`pg_stat_archiver` describes WAL archiving. It is real evidence about a real thing, and it does not
answer "did last night's backup run". PostgreSQL keeps no record of its own backups at all.

What a PostgreSQL estate does have is a directory somebody's cron job writes into, and that is what
the plugin reads — the same directory
[ADR-0026](../../adr/0026-a-shared-directory-carries-a-file-based-artifact.md) already put in the
contract for SQL Server's artifact handover, which is why that ADR said this work would land on the
same ground.

**A directory can prove exactly one thing: a file arrived, this big, at this time.** A truncated
`pg_dump` leaves a file behind exactly as a complete one does, so the source can never report
success, and it says so rather than being allowed to imply otherwise:

```go
ReportsOutcome: false          // every record is OBSERVED_OUTCOME_UNKNOWN
IdentityIsEngineAssigned: false // a renamed file is a new file
```

Both flags reach the row, the adherence answer, and the caveat printed under it. A window satisfied
only by a file is `UNPROVEN`, never `ADHERENT` — a fourth state that exists precisely so the third
one keeps its meaning.

The identity digests **the file's name and nothing else** — not its size, not its modification time.
That choice is what makes a dump still being written while a poll runs update one record on the next
poll rather than insert a second, and it makes a file written to the same path every night one
record whose finish time moves. What it cannot survive is a rename, which is what the declared flag
is for.

## The correctness trap, and the thing that only a real instance would have told us

`msdb.dbo.backupset` stores `datetime`, in the **server's local time**, with no offset anywhere.
Comparing that to a UTC compliance window without knowing the offset produces adherence answers
wrong by an hour twice a year, in the direction nobody checks. So the plugin converts, and the
contract's timestamps are UTC by definition.

The conversion that is right is `AT TIME ZONE <zone> AT TIME ZONE 'UTC'`, which applies the offset in
force *on the day of the backup* rather than today's. Naming the zone is what makes it exact. And
this is where the measurement mattered:

```
CURRENT_TIMEZONE()     → "(UTC) Coordinated Universal Time"
CURRENT_TIMEZONE_ID()  → "UTC"
```

The first returns a **display name**, which `AT TIME ZONE` rejects outright:

```
The time zone parameter '(UTC) Coordinated Universal Time' provided to AT TIME ZONE clause is invalid.
```

That failure was found by the conformance case on the first run, and it would not have been found by
reading the documentation — both functions are documented as returning "the time zone", and only one
of them returns the identifier. `CURRENT_TIMEZONE_ID` arrived in SQL Server 2022, so an instance
older than that cannot name its zone in a usable form at all, and there is no supported mapping from
the display name to the identifier: the one that exists reads the Windows registry, which this
plugin will not do.

Two things came out of it, and both are better than the code would have been without the failure.

**The plugin verifies the zone against the engine before depending on it.** One cheap
`SELECT SYSDATETIME() AT TIME ZONE @tz` turns "this instance names its zone in a way we cannot use"
from a failed poll into a degraded answer — which is what `finished_at_is_approximate` is for, and
what makes that field load-bearing rather than theoretical. An instance on SQL Server 2019 takes the
current-offset path, is wrong by at most one daylight-saving transition, and says so per record; core
widens the compliance window by exactly that much for those records and reports the caveat.

**The error classification was masking a query failure as a connection failure.** The first
diagnosis of the above was `connect to the instance`, because everything the engine answers with
arrives as an error from a connection and the fallback swallowed it. `classifyHistoryError` now has
three branches — permission, connection, and *the engine answered and what it said was no* — and the
third is the one that saves an afternoon. Reporting a rejected query as an unreachable server sends
somebody to check a firewall that is fine.

## The trap that only appears once

**`tools/docsgen/schema.go` read `000001_init.up.sql` and nothing else**, and understood only
`CREATE TABLE`. A second migration would therefore have been invisible to the generated data-model
document — silently, because a generated file that is stable and wrong diffs clean against itself,
and CI would have passed.

Fixed as part of the slice: every `*.up.sql` is read in order, and `ALTER TABLE … ADD COLUMN` is
applied to the table it names. The proof is a diff rather than an assertion:

```
| `schedules` | 16 → 18 |
| `backups`   | 24 → 29 |
```

Before the fix that diff was empty.

**And the down migration is not the mirror image of the up.** Narrowing a `CHECK` makes PostgreSQL
validate it against the rows already there, so a down migration that only swaps the constraint back
fails on exactly the rows its own up migration made possible. `000002_observed_backups.down.sql`
deletes the observed backups, observation schedules and observation jobs first, deliberately: an
observed backup is evidence Fleetward read from an engine and can read again on the next poll, and
an observation schedule would have nothing to run it. Neither is data only Fleetward holds.
`TestMigrationsRunBothWays` seeds exactly those rows and then rolls all the way back and forward
again, because rolling back an empty schema proves nothing.

## Decisions worth carrying forward

**An observed backup is refused verification at the boundary, and for the right reason.** It carries
no manifest, so it would otherwise fall into `verify.go`'s manifest-less branch and come back
`INCONCLUSIVE` — an answer that reads as "the check went wrong" and sits in the same bucket as an
image that could not be pulled. It is not that. It is permanent and structural, so the refusal
happens before a job or a verification row exists, in the only place where saying it changes
anything: to the person asking. No new `VerificationStatus` was added; `origin` already says it.

**Adherence is computed on read and stored nowhere**
([ADR-0028](../../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md)). Two
queries answer for the whole estate, there is no cache to invalidate after a schedule edit, and B7's
alert rule will read the same computation rather than a table it would have to trust.

**The expectation is declared, never inferred.** Fleetward could have derived it from the observed
rhythm, and that answers *is this normal for you* — so a server that quietly stopped backing up in
March would have the failure normalised into its own baseline. The product exists to answer *is this
what you asked for*.

**The occurrence under judgement is the last one whose grace has already expired.** An instance
expected to back up at 02:00 with two hours of grace is not reported as behind at 02:30 while the
backup may still be running. A declaration with no tolerance gets two hours rather than zero, because
zero demands a backup complete in the instant it was due and would report the entire estate as
missing one.

**Observation is a job kind rather than a second timer.** Everything at-most-once about it — the
claim, the heartbeat, the lease that can cancel the work — comes free from B1's machinery. The cost
was one `Runner` method, one adapter file, and two widened `CHECK` constraints. Both constraints,
not one: `schedules.kind` and `jobs.kind` each enumerate their values, and widening only the first
produces a scheduler that creates schedules it can never run.

## Not built, deliberately

- **Alerting on a missed window.** The answer is computed and served and delivered nowhere. **B7.**
- **Any UI.** No screen and no `web/` change. **B4.**
- **Retention or expiry of observed backups.** Fleetward does not own the artifact and must not
  delete it. **B5.**
- **Importing an observed artifact into the object store.** Taking ownership of somebody else's file
  is a different product decision and needs its own record.
- **Verifying an observed backup.** Refused, deliberately.
- **`pg_stat_archiver` and WAL continuity as evidence.** A different question; it belongs with PITR.
- **Per-database expectations.** One per instance. A second consumer needs the separate table
  ADR-0028 declined to build yet.
- **Authorization on the new routes.** Every route under `/api/v1/` is open. **B6.**
- **MySQL, MongoDB, Redis.** They still only handshake. **B11–B16.**

## Still open

- **The observation horizon and overlap are constants, not configuration.** A first poll reads thirty
  days back and every poll re-reads six hours. Both are named and commented in
  `internal/controlplane/backup/observe.go`. Making them configurable is speculative until somebody
  has an estate that needs different values, and the smaller surface is the deliberate choice —
  but it is a choice, and it is written down here rather than left to be discovered.
- **A renamed backup file appears as a second backup**, on any source whose identity is derived. It
  is declared and reported rather than papered over with matching heuristics core would have to
  invent, which is the honest trade and not a free one.
- **An instance on SQL Server 2019 or older reports approximate finish times.** Correct within an
  hour, flagged, and the compliance window is widened to match. An exact answer there needs a mapping
  from a display name to a time-zone identifier that SQL Server does not expose without the registry.
- **The DST conversion is exercised by construction rather than by assertion.** Both the conformance
  runner and the sandbox containers run in UTC, so the case cannot distinguish a correct conversion
  from an absent one. What is asserted is the widening it drives, in
  `internal/controlplane/backup/adherence_test.go`, and the failure that found the problem was a hard
  error rather than a wrong number — which is the failure mode this design was built for.
- **Two integration tests fail on this development machine and neither is a regression.** Both are in
  `STATUS.md`'s environment notes and both were reproduced on `main` before B1.
