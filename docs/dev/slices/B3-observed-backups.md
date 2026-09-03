# Slice B3 — Observed backups, and the first answer Fleetward can give on day one

## Goal

Point Fleetward at an estate whose backups are already being taken by something else, change nothing
on it, and get the answer to **"did every server's backup run on schedule, and did it succeed"**.

## Why now

Fleetward can currently report only on backups it took itself. The user this product exists for has
roughly fifty servers whose backups are already being taken — by cron, by scripts, by native
scheduling, by tooling that predates Fleetward.
[ADR-0015](../../adr/0015-observed-and-managed-backups.md) is blunt about the consequence: a tool
that demands you migrate fifty production servers' backup arrangements before it shows you anything
useful does not get adopted, because migrating production backup arrangements is what one does
*after* trusting a tool, not in order to start trusting it.

This is the first pillar of the product thesis — declare what should be true, detect what actually
is, show the gap — and it is the only pillar answerable for an entire estate without owning a single
artifact.

**B2 came before it deliberately.** SQL Server records every backup taken on an instance in
`msdb.dbo.backupset`: identity, start, finish, size, type, outcome, per database. It is by far the
richest observable backup history of any engine on the roadmap. PostgreSQL has the poorest —
`pg_stat_archiver` describes WAL archiving, not files somebody wrote to a directory, and answers a
different question entirely. Designing `ListBackupHistory` with the richest source and the poorest
source both in hand is how it avoids being a SQL-Server-shaped RPC wearing a generic name.

> **For the product:** after this slice a DBA adds fifty instances, declares when each one's backup
> is supposed to happen, and gets one list of which servers are behind. Nothing on those servers
> changes: no agent, no new credential, no change to how their backups are taken. What they get is
> the answer they cannot currently get in a working week.

## Preconditions

All of these already hold. They are listed so a session started out of order notices.

- **Two real engines pass conformance.** PostgreSQL and SQL Server implement health, discovery,
  backup, sandbox restore, and verification, and five Stage 1 conformance cases run against both.
- **The scheduler runs.** B1 built cron materialization, at-most-once claiming by lease, heartbeat,
  and recovery after a crash, and none of that machinery is specific to the `backup` kind.
- **A shared directory already exists in the contract.**
  [ADR-0026](../../adr/0026-a-shared-directory-carries-a-file-based-artifact.md) added
  `Credentials.shared_directory` with its two paths, and `fleetward-cli instance add` already carries
  `--backup-dir` and `--backup-dir-local`. That ADR says in as many words that this is the ground the
  observed-backup work would land on, and it is.
- **`ListBackupHistory` does not exist**, and neither does any capability describing what a plugin
  can observe. Verified in `api/proto/fleetward/v1/plugin.proto`.
- **`backups.origin` does not exist.** The `backups` table carries `triggered_manually`, `job_id`
  and `schedule_id`, and nothing that distinguishes an origin.
- **This is the first migration since the initial schema.** `internal/storage/metadb/migrations/`
  holds only `000001_init.up.sql` and its down file. That has consequences — see trap 1.

## Design decisions already made

Settled before this brief was written, mostly by ADR-0015. They are recorded so they are not
relitigated, which is this project's expensive failure mode.

### 1. Two origins, and the distinction runs through the contract, the schema and the UI

`observed` is evidence of a backup Fleetward did not take. `managed` is one it ran. ADR-0015 decided
this; this slice implements it.

### 2. Only managed backups can be verified, and the refusal is a correctness requirement

An observed backup has no `SourceManifest` captured at backup time, so its *contents* cannot be
attested to. ADR-0015 calls the two-part status display a correctness requirement rather than a UI
nicety, and the same applies to the refusal underneath it: a verification of an observed backup must
not be attempted, and must not be reported in a way that reads like an infrastructure hiccup. See
[open decision 5](#5-what-happens-when-someone-asks-to-verify-an-observed-backup).

### 3. Adherence applies equally to both origins

Did a run happen inside its window, did it succeed. That question does not care who took the backup,
and answering it for an entire estate on day one is the whole point of the slice.

### 4. What a plugin can observe is a capability, not an assumption

Engines differ enormously here. The capability is what core branches on, and the acceptance test is
unchanged: no engine name appears in `internal/` or `web/`.

### 5. A capability-gated conformance case is allowed here, and is not what `CLAUDE.md` forbids

The rule is that an **engine** must never force a change to the shared suite. A new RPC in the
contract legitimately gets its own case, skipped by every plugin that does not declare it. What must
not move is an existing assertion. This slice adds one new case file and the fixture hook it needs;
if anything else under `test/conformance/` has to change, that is a finding, and the finding is the
result.

### 6. Timestamps crossing the contract are UTC, always

`google.protobuf.Timestamp` has no other meaning. The burden of converting an engine's local-time
record is the plugin's, and where the plugin cannot convert exactly it says so per record rather than
silently. See trap 3, which is a correctness trap and not a formatting one.

---

## Open decisions

**No code is written against any of these until they are approved.** Each carries its consequences
and a recommendation.

### 1. What identifies an observed backup, so a poll does not insert it twice

This is B3's version of B2's transport question, and whatever is chosen becomes an upsert key in the
schema and a required field in the contract.

Every poll re-reads the same source, so a record needs a stable identity **that came from the engine
rather than from us**. The two sources this slice implements have very different answers:

| Source | What it can offer as identity | Survives |
|---|---|---|
| `msdb.dbo.backupset` | `backup_set_uuid`, a real `uniqueidentifier` the engine assigns | anything short of deleting the row |
| a directory listing | path, size, modification time | not a rename, not a move |

**Option A — core derives identity from a tuple of fields** it already understands: instance,
database, start time, method. Needs no contract field. Fails on an engine that records two backups in
the same second, and re-inserts every row the moment an engine corrects a timestamp.

**Option B — a hash of the whole record.** Any change at all produces a new identity, so a backup
observed while running and then observed as finished becomes two rows. Wrong by construction.

**Option C — a plugin-supplied `external_id`, opaque to core, unique within the instance.** The
plugin composes it from whatever the engine offers, and guarantees two properties: stable across
polls, unique within the instance. Core upserts on `(instance_id, external_id)` and never inspects
the string. SQL Server returns `backup_set_uuid`; the PostgreSQL directory source returns a digest of
the path.

**Recommendation: Option C**, plus one declared quality — because a `uniqueidentifier` and a filename
digest are genuinely different promises, and Fleetward should not present them as the same:

```proto
// BackupHistoryCapabilities
bool identity_is_engine_assigned = 3;
```

When it is false, Fleetward's own reporting says that a moved or renamed file may appear as a second
backup. That is a caveat a DBA can act on; a silent duplicate is not.

#### 1b. And what happens when Fleetward's own backup appears in the engine's history

Found while reading `msdb`. It is the same mechanism, so it is decided here.

A Fleetward-managed SQL Server backup writes a row into `msdb.dbo.backupset` like any other. The next
poll therefore sees the backup Fleetward itself took, and inserts it a second time as `observed`. One
physical backup, two rows, one of them claiming an origin it does not have.

**Option A — accept it.** Adherence still answers correctly, because a window is satisfied by any
backup. The cost lands entirely on the human: a history list showing every managed backup twice is a
list a DBA stops trusting, and they would be right to.

**Option B — core matches observed evidence against managed rows** on time, database and size. Fuzzy
matching invented by core, wrong at the edges, and exactly the kind of engine-shaped guessing the
capability matrix exists to replace.

**Option C — the managed backup records the engine's own identifier when it has one.** One more
additive field, `BackupResult.external_id`, populated by the plugin that just took the backup and
knows what the engine called it. The unique index then covers both origins, and the observed poll's
upsert lands on the row that is already there. PostgreSQL leaves it empty and nothing changes for it.

**Recommendation: Option C.** It is one field, it reuses the identity decided above rather than
inventing a second mechanism, and it converges rather than de-duplicates — there is never a moment
where two rows exist and something has to choose between them.

### 2. What PostgreSQL can actually observe

`pg_stat_archiver` describes WAL archiving. It is real evidence about a real thing, and it does not
answer "did last night's backup run".

**Option A — declare that this plugin cannot observe backups at all.** `supported = false`,
`ListBackupHistory` refuses with a typed `UNSUPPORTED`. Completely honest, and it exercises the
capability mechanism in the direction that matters — a plugin declining. It also leaves the slice with
one engine, which means the RPC is designed against exactly the single source the roadmap warns about
designing against alone.

**Option B — list a configured backup directory.** Files matching a pattern in the directory ADR-0026
already put in the contract. This is what a large share of PostgreSQL estates actually have: a cron
job writing a dump per night to a path. The evidence is weak and honestly so — a file exists, it is
this big, it was last written at this time — and in particular **a truncated dump leaves a file too**,
so the outcome is genuinely unknown rather than successful.

**Option C — report archiving evidence and be explicit that it is a different question.** It belongs
with PITR, where WAL continuity is the question actually being asked. Not here.

**Recommendation: Option B, with Option A's honesty built into the record rather than bolted on.** The
observed outcome carries an explicit `UNKNOWN`, and the capability declares `reports_outcome = false`
for this source, so nothing downstream can render a directory listing as "succeeded". The poorest
source in the estate is the one that stops this RPC from being SQL-Server-shaped, and refusing to
implement it would leave that untested.

> **For the product:** for PostgreSQL, Fleetward answers "a backup file arrived last night, and it was
> about the size it usually is". It does not claim the backup was good. That is worth having, it is
> worth being told plainly, and it is the honest ceiling of what a directory can prove.

### 3. How the poll is driven

`internal/controlplane/scheduler/service.go` currently refuses every schedule kind but `backup`, with
a message saying the others "arrive with the estate view" — B4.

**Option A — on demand only.** An endpoint and a CLI command that polls one instance. Cheapest. It
also fails the slice's own headline: "did every server's backup run" cannot require a human to type a
command per server, on an estate of fifty, which is the estate this exists for.

**Option B — unlock a schedule kind.** A new `observe` kind, materialized and leased by exactly the
machinery B1 built. One new runner, and a migration that widens two `CHECK` constraints. On-demand
comes free alongside it, because both paths call the same service method — the shape `RunBackup`
already has.

**Recommendation: Option B.** Observation is recurring work that has to happen without anyone asking,
and the infrastructure for recurring work exists and is tested. The cost is one runner and two
constraint changes, in a migration this slice is writing anyway.

The kind is named **`observe`**. `backup_history` describes the source rather than the work.

### 4. Where the declared expectation lives

Not on your list, and it has to be answered, because adherence needs both halves. Detection is what
this slice builds; **declaration is what makes the gap computable**, and inferring an expectation from
the observed rhythm would be exactly the wrong shape for a product whose thesis is that you declare
what should be true.

**Option A — two columns on `schedules`**, meaningful on `observe` rows: `expected_cron` and
`expected_grace_minutes`. The schedule's own `cron_expression` stays what it is, the poll cadence; the
new pair declares when a backup is supposed to have happened, and how late is still acceptable. One
migration, and B1's cron evaluator is reused for both.

**Option B — a `backup_expectations` table**, one row per instance, later per database. More room to
grow, more schema, and no second consumer yet.

**Option C — no declaration in B3**, report history only. Shrinks the slice below its own headline.

**Recommendation: Option A.** Two crons on one row want a clear comment, and they earn it: the row
says "look here this often, and expect a backup this often". A per-database expectation would need
Option B, and Option B is reachable from Option A by a migration once a second consumer exists.

**And adherence is computed on read, not stored.** Nothing writes an adherence verdict. The endpoint
walks instances carrying an expectation, computes the last expected occurrence from the cron in the
schedule's timezone, and asks whether a backup of either origin satisfied it inside the grace period.
There is no staleness to manage and no reconciliation, and B7's alert rule later reads the same
computation rather than a table it would have to trust.

### 5. What happens when someone asks to verify an observed backup

`internal/controlplane/backup/verify.go` refuses a manifest-less backup as `INCONCLUSIVE`, and the
reason it gives is right for the case it was written for: comparing zero objects to zero objects
succeeds trivially, so an absent manifest must never reach a sandbox. An observed backup falls into
that branch by construction and gets an answer that reads like an infrastructure hiccup.

**Option A — a new `VerificationStatus`,** `NOT_APPLICABLE`. Precise. It also enlarges a set that is
deliberately three values wide, changes a `CHECK` constraint, and gives the UI a fourth state to
render for something `origin` already says.

**Option B — refuse at the boundary.** `RunVerification` on an observed backup returns
`INVALID_ARGUMENT` and creates neither a job nor a verification row; the scheduler never enqueues a
verify job for one; and a guard stays inside `runVerification` for the path that does not come through
the API, reporting the structural reason rather than the manifest one.

**Recommendation: Option B.** "This is not a thing that can be verified" is a permanent, structural
answer, and putting it in the same bucket as "the image could not be pulled" trains an operator to
ignore the bucket. Refusing before a row exists says it in the only place where saying it changes
anything: to the person asking.

---

## What the contract gains

All additive, so `buf breaking` stays green, and none of it puts an engine's name in core.

```proto
// plugin.proto — the RPC
rpc ListBackupHistory(ListBackupHistoryRequest) returns (ListBackupHistoryResponse);

message ListBackupHistoryRequest {
  ConnectionRef connection = 1;
  Credentials credentials = 2;
  // Only backups that finished at or after this instant. Core sets it from its own watermark.
  google.protobuf.Timestamp since = 3;
  // Maximum records to return. The plugin MUST honour it: an engine's own history table on an
  // instance up for years holds hundreds of thousands of rows, and a full scan of it on every poll
  // is not acceptable against a production server.
  int32 limit = 4;
  string page_token = 5;
  repeated string databases = 6;
  google.protobuf.Duration timeout = 7;
}

message ListBackupHistoryResponse {
  repeated ObservedBackup backups = 1;
  string next_page_token = 2;
}

message ObservedBackup {
  // Stable within this instance and assigned by the engine, not by the moment of observation.
  // Core upserts on it and never inspects it.
  string external_id = 1;
  string database = 2;
  // The method as the engine names it: "database", "log", "differential", "pg_dump".
  string method = 3;
  BackupKind kind = 4;
  ObservedOutcome outcome = 5;
  google.protobuf.Timestamp started_at = 6;   // UTC, always
  google.protobuf.Timestamp finished_at = 7;  // UTC, always
  // The plugin could not convert the engine's own record to UTC exactly — see trap 3. The default,
  // false, means exact, so a source that records an offset keeps its meaning.
  bool finished_at_is_approximate = 8;
  int64 size_bytes = 9;
  // Where the artifact is, as the engine or the filesystem names it. Fleetward does not own it and
  // never reads it.
  string location = 10;
  map<string, string> details = 11;
}

enum ObservedOutcome {
  OBSERVED_OUTCOME_UNSPECIFIED = 0;
  OBSERVED_OUTCOME_SUCCEEDED = 1;
  OBSERVED_OUTCOME_FAILED = 2;
  // A backup happened. The evidence says nothing about whether it completed correctly.
  OBSERVED_OUTCOME_UNKNOWN = 3;
}

// Capabilities gains one field.
BackupHistoryCapabilities backup_history = 26;

message BackupHistoryCapabilities {
  bool supported = 1;
  // Display only. Core must not branch on it.
  string source_description = 2;
  bool identity_is_engine_assigned = 3;
  // The evidence distinguishes a successful backup from a failed one. When false, every record is
  // OBSERVED_OUTCOME_UNKNOWN and nothing downstream may render it as success.
  bool reports_outcome = 4;
  // Observation needs the directory of ADR-0026, so core refuses an observe schedule on an instance
  // that has none — the same precondition check BackupMethod.requires_shared_directory already has.
  bool requires_shared_directory = 5;
}

// BackupResult gains one field, for open decision 1b.
string external_id = 12;   // next free tag; confirm against the file
```

## Files

### New

| Path | What |
|---|---|
| `internal/storage/metadb/migrations/000002_observed_backups.up.sql` | `origin`, the identity index, the expectation columns, two widened `CHECK`s |
| `internal/storage/metadb/migrations/000002_observed_backups.down.sql` | the reverse, including the rows that block it — trap 2 |
| `internal/controlplane/backup/observe.go` | one poll: watermark, plugin call, paged upsert |
| `internal/controlplane/backup/adherence.go` | the read-time evaluation, stored nowhere |
| `internal/controlplane/scheduler/observerunner.go` | the `observe` job kind, beside `backuprunner.go` |
| `plugins/sqlserver/history.go` | the engine's own history table, converted to UTC and bounded |
| `plugins/postgres/history.go` | a configured directory, and an honest `UNKNOWN` |
| `test/conformance/history_test.go` | one capability-gated case |
| `docs/adr/` | identity and origin convergence; observation as a schedule kind |
| `docs/dev/journal/` | the close-out entry |

### Modified

| Path | Why |
|---|---|
| `api/proto/fleetward/v1/plugin.proto` | the RPC, its messages, the capability, `BackupResult.external_id` |
| `api/proto/fleetward/v1/controlplane.proto` | `Backup.origin`, a new job kind, the adherence RPC, a `ListBackups` origin filter |
| `api/gen/fleetward/v1/` | regenerated and committed — `make proto` |
| `api/openapi/` | regenerated |
| `internal/plugin/sdk/base.go` | a typed refusal for the new RPC, so every existing plugin still satisfies the contract |
| `internal/plugin/sdk/capabilities.go` | validation of the new capability block |
| `internal/controlplane/backup/service.go` | the observed-origin write path, and the `ListBackups` filter |
| `internal/controlplane/backup/verify.go` | refuse an observed backup for the structural reason |
| `internal/controlplane/backup/grpc.go` | the adherence RPC |
| `internal/controlplane/scheduler/service.go` | accept the new kind; check the shared-directory precondition |
| `internal/controlplane/scheduler/scheduler.go` | one more job kind through the existing lease machinery |
| `internal/controlplane/scheduler/runner.go` | materializing the new kind |
| `cmd/fleetward-cli/backup.go` | `backup history`, `backup adherence` |
| `cmd/fleetward-cli/schedule.go` | creating an observe schedule with its expectation |
| `tools/docsgen/schema.go` | read every migration, not just the first — trap 1 |
| `docs/dev/data-model.md` | regenerated, and this is how trap 1 is proved fixed |
| `docs/engines.md` | what each of the two engines can observe |
| `docs/ops/scheduling.md` | the second schedule kind, and the expectation |
| `docs/dev/writing-an-engine-plugin.md` | the new RPC and capability block |
| `README.md`, `CLAUDE.md` | if the layout trees or the described behaviour move |
| `docs/dev/STATUS.md` | rewritten |
| `docs/.docscheck-allow` | the allowances this brief adds, removed as each file appears |

> Core files appear in that list, and that is not a failure of the claim B2 established. The claim is
> that core needs no **engine-specific knowledge** — no name, no lookup table, no branch. Generic code
> consuming a new declarative field is the sanctioned fix, and the acceptance test for it is a `grep`
> that returns nothing.

## Reuse, do not rewrite

- **`internal/controlplane/scheduler/backuprunner.go`** — the shape of a runner: claim, heartbeat, bind
  the work to the lease's context, record the outcome. `observerunner.go` is the same shape with a
  different body, and must not invent a second one.
- **`internal/controlplane/scheduler/cron.go`** — `nextRun(expr, timezone, now)` already parses and
  evaluates a cron in a timezone, with tests. The expectation's cron uses it; do not add a second cron
  dependency.
- **`internal/controlplane/inventory/credentials.go`** — `sharedDirectory()` already reassembles the
  contract message from the stored connection options. The observe path resolves credentials the same
  way every other path does.
- **`internal/controlplane/backup/service.go`** — how a `backups` row is written, and how a plugin
  client is obtained from the manager.
- **`internal/plugin/sdk`** — every error constructor. A plugin never returns a bare error, and the
  refusal for an unimplemented RPC already exists on `Base`.
- **`test/conformance/fixture_postgresql_test.go`** and its SQL Server sibling — the shape and the size
  a fixture is allowed to be
  ([ADR-0023](../../adr/0023-conformance-fixtures-seed-what-the-contract-cannot.md)).
- **`internal/controlplane/scheduler/service.go`'s precondition check** — B2 already refuses a schedule
  whose method needs a shared directory the instance does not have, and names which half is missing.
  The observe schedule reuses that; it does not grow a parallel one.

## Traps

Ordered by how much time each costs if it is found late.

1. **`tools/docsgen/schema.go` hardcodes `000001_init.up.sql`.** A second migration is therefore
   invisible to the generated data-model document, silently — `make docs` regenerates a diagram missing
   every new column, and CI diffs it happily, because it is stable and wrong. It also understands only
   `CREATE TABLE`, so `ALTER TABLE … ADD COLUMN` would be skipped even if the file were read. Fixing
   both is part of this slice, and it is exactly the kind of defect that appears the first time somebody
   adds a migration — which is now. **Prove it by regenerating and reading the diff:** `origin` must
   appear on `backups` in `docs/dev/data-model.md`.
2. **A down migration that narrows a `CHECK` fails on the rows that need it.** `000002` widens
   `backups.state` to admit an unknown outcome, and `schedules.kind` and `jobs.kind` to admit the new
   kind. Reversing that is not `DROP CONSTRAINT` plus `ADD CONSTRAINT` — it is that plus deciding what
   happens to rows already using the new values. Decide it deliberately and write it into the file:
   the down migration deletes observed backups and observe schedules, because they are re-derivable by
   another poll and a half-applied constraint is not.
3. **An engine's own backup history is often stored in the server's local time, with no offset.** SQL
   Server's is a `datetime`. Comparing that to a UTC window without knowing the instance's offset
   produces adherence answers wrong by hours, twice a year, in the direction nobody checks. This is a
   correctness trap, not a formatting one.
   The conversion that is right is `AT TIME ZONE CURRENT_TIMEZONE() AT TIME ZONE 'UTC'`, which uses the
   historical offset for that date rather than today's — so a July backup read in January converts
   correctly. `CURRENT_TIMEZONE()` needs SQL Server 2019 or newer. On an older instance the only thing
   available is the *current* offset from `SYSDATETIMEOFFSET()`, which is wrong across a DST boundary by
   exactly one hour: that is what `finished_at_is_approximate` is for, and a record carrying it must
   widen the adherence comparison rather than be silently trusted.
4. **That history table can hold hundreds of thousands of rows.** An instance up for years with no
   history cleanup accumulates a row per database per backup per type, forever. A full scan on every
   poll is not acceptable against a production server: the query needs a bounded page ordered by finish
   time, filtered by `@since`, and core needs a watermark to supply it.
   **Derive the watermark rather than storing it** — `max(completed_at)` over that instance's observed
   backups, minus a fixed overlap window. Re-reading the overlap is free precisely because the identity
   of open decision 1 makes a repeated record an upsert; and a derived watermark cannot go stale, cannot
   be lost in a restore of the metadata database, and self-heals after a missed poll. The first poll on
   a new instance has no watermark: bound it by a configured horizon rather than reading the instance's
   entire history.
5. **An observed backup has no manifest, by definition.**
   [ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md) makes a manifest-less
   backup `INCONCLUSIVE` by construction — `verify.go` does this already and is right to. An observed
   backup falls into that branch and gets told what looks like an infrastructure story. Open decision 5
   is how this is answered; the trap is that doing nothing produces a *plausible* wrong answer rather
   than an error.
6. **A capability-gated conformance case is allowed; editing an assertion is not.** Adding a case for a
   new RPC, skipped by every plugin that does not declare it, is the suite working as designed. The rule
   `CLAUDE.md` states — an engine must never force a change to the suite — is about engines. Say which
   of the two you are doing before you do it.
7. **The conformance case needs a backup the fixture caused, not one Fleetward took.** For SQL Server
   the managed backup already lands in the engine's history, so the case could run against the existing
   path. For a directory source it cannot: a managed `pg_dump` streams to the object store and leaves
   nothing on the share. The `Fixture` interface therefore gains one optional hook — cause a backup by
   the engine's own means — implemented only by fixtures whose plugin declares observation. That is a
   change under `test/conformance/`, it is sanctioned by trap 6, and it is the only one.
8. **The idempotence assertion is the one that matters.** A case that calls `ListBackupHistory` once and
   finds a record proves almost nothing. The case that protects open decision 1 calls it twice and
   asserts the same `external_id` comes back — then upserts twice through core and asserts one row.
   Write that one first.
9. **Two `CHECK` constraints, not one.** `schedules.kind` and `jobs.kind` both enumerate their values. A
   schedule kind that materializes into a job kind needs both, and forgetting the second produces a
   scheduler that creates schedules it can never run.
10. **`buf breaking` runs against `main`.** Everything above is additive. Confirm the next free field tag
    in each message from the file rather than from this brief.
11. **`make` is not installed on this machine.** Run the targets directly, and say so rather than
    reporting `make lint test` as passing.
12. **On Windows, `gofmt -l` and `buf format --diff` report every file in the tree.** It is a
    `core.autocrlf=true` artefact, not a finding. Verify in a worktree created with
    `git -c core.autocrlf=false worktree add`. `go test -race` needs cgo, which a stock install does not
    have; CI runs `-race` on Linux.
13. **Run `go mod tidy` before pushing.** A dependency added with `go get` before it is imported stays
    `// indirect`, and building, testing and linting are all happy with that. CI is not. This has cost a
    full CI cycle before.
14. **Two integration tests fail on this machine and neither is a regression.** Both are written up in
    `docs/dev/STATUS.md`'s environment notes and both were reproduced on `main`. Do not chase them.
15. **The docscheck allowances this brief adds are self-reporting.** `docs/.docscheck-allow` gains an
    entry per file the Files table names that does not exist yet, scoped to this brief. Each is removed
    in the change that creates its file.

## Scope fence

Explicitly **not** in this slice. A session reading the roadmap will want to build all of it.

- **Alerting on a missed window.** The adherence answer is computed and served; nothing is delivered
  anywhere. **B7.**
- **Any UI.** No screen, no `web/` change. **B4.**
- **Retention or expiry of observed backups.** Fleetward does not own the artifact and must not delete
  it. **B5.**
- **Importing an observed artifact into the object store.** Taking ownership of somebody else's file is
  a different product decision and it needs its own ADR.
- **Verifying an observed backup.** Refused, deliberately, by open decision 5.
- **MySQL, MongoDB, Redis.** They still only handshake. **B11–B16.**
- **`pg_stat_archiver` and WAL continuity as evidence.** A different question, and it belongs with PITR.
- **Per-database expectations.** One expectation per instance. Open decision 4 records how the second
  one arrives.
- **Authorization on the new routes.** Every route under `/api/v1/` is open today. Adding one does not
  change that and must not pretend to. **B6.**
- **Observed restores.** Whether somebody else restored something is a real question, and it is not this
  one.

## Done when

Concrete commands, with the output that counts as a pass.

```bash
# The claim that survives every slice. Must come back empty.
grep -rniE "sqlserver|mssql|postgres" internal/ web/src/ \
  | grep -v "_test\.go:" | grep -vE ":[0-9]+:\s*//"

# The new case runs for both real engines and skips for the three that only handshake.
go test -tags=conformance -timeout 60m -v ./test/conformance/... \
  | grep -E "^(=== RUN|--- (PASS|FAIL|SKIP))"
#   → TestBackupHistoryIsObservable/postgresql PASS
#   → TestBackupHistoryIsObservable/sqlserver  PASS
#   → the other three SKIP with a reason naming the capability

# Trap 1, proved rather than asserted.
go run ./tools/docsgen && git diff --stat docs/dev/data-model.md
#   → non-empty, and `origin` appears on the backups entity

# The rest of the tree.
golangci-lint run
go test ./...                                # in a core.autocrlf=false worktree
go test -tags=integration ./internal/...
buf lint && buf format --diff --exit-code && buf breaking --against '.git#branch=main'
go mod tidy && git diff --exit-code go.mod go.sum
go run ./tools/docscheck
```

And the walk, which is the part a test suite cannot do: `docker compose up --build`, two instances
added, a backup taken **outside Fleetward entirely** — one typed by hand into the engine, one written
into the other instance's directory by hand — then an observe schedule, and then nobody typing
anything.

```
fleetward-cli backup history --instance <name>
ID        ORIGIN    OUTCOME    DATABASE  FINISHED (UTC)       SIZE
…         observed  succeeded  …         …                    …
…         managed   succeeded  …         …                    …

fleetward-cli backup adherence
INSTANCE  EXPECTED       GRACE  LAST BACKUP (UTC)    ADHERENCE
…         0 2 * * *      2h     …                    adherent
…         0 2 * * *      2h     —                    missed
```

with `fleetward-cli backup verify <an observed backup>` refusing in one sentence that says why, and
`docs/dev/data-model.md` showing the new columns.

Then the four close-out artefacts from [`README.md`](README.md): the journal entry, `STATUS.md`
rewritten, `docs/engines.md` updated, and the ADRs that open decisions 1 and 3 produce.
