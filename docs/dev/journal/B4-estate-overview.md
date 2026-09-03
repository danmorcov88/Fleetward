# B4 — The Estate Overview, and the document that had been wrong all along

- **Delivered:** 2026-09-03
- **Brief:** [B4-estate-overview.md](../slices/B4-estate-overview.md)

Until this slice everything Fleetward knew was reachable only by typing a command per server. B3 had
finished the computation the product exists for — did every server's backup run when it was supposed
to, and did it succeed — and left it behind `fleetward-cli backup adherence`, which is a fine surface
for one server and a chore for fifty.

**The deliverable is one screen with four columns**, and the two questions that matter answerable
without clicking anything. Three things arrived with it that are not the screen: an API client
generated from the contract, a `discovery` schedule kind so the health column moves on its own, and
a test runner for the UI.

## How it was verified

On Windows (amd64), 2026-09-03, against Go 1.25.6, Node 24, buf 1.58.0, Docker 27.3.1,
`postgres:16-alpine` and `mcr.microsoft.com/mssql/server:2022-latest`.

```
go vet ./...                    ok
go vet -tags=integration ./...  ok
go vet -tags=conformance ./...  ok
go test ./...                   ok, no failures
npm run lint                    clean
npm run test                    2 files, 20 tests, passed
npm run build                   379 kB, 119 kB gzipped
go run ./tools/docscheck        67 markdown files, no problems
go mod tidy                     no drift
buf lint / format / breaking    clean
```

**`go vet -tags=integration` earned its place in the brief immediately.** Widening
`scheduler.Runner` with a fourth method broke `stubRunner` in `integration_test.go` — a file this
slice had no reason to open, in a package it did — and only a tagged vet compiles it. B3 paid a full
CI cycle for exactly this; B4 paid eight seconds.

## The finding that reframed the slice

The brief was written expecting to generate a client from `api/openapi/openapi.yaml`. Reading the
document first is what saved the slice: **it did not describe the API the control plane serves.**

It had been generated, committed and diffed by CI since the contract existed. That made it stable,
and stable is not the same as correct:

| The document said | The server sends |
|---|---|
| `instanceId` | `instance_id` |
| `state: {type: integer}` | `"ADHERENCE_STATE_MISSED"` |
| `google.rpc.Status` errors | RFC 9457 problem details |
| no `/readyz` | `/readyz` |

Generating from it would have produced a client that compiles and reads nothing — every field
`undefined`, every enum comparison false. The same shape of failure `tools/docsgen` had before B3
fixed it, and this one would have been discovered by a DBA looking at four blank columns.

Two generator options fixed the first two rows, and both were verified against the installed
toolchain before being written into the brief rather than assumed from documentation:
`naming=proto,enum_type=string`. The other two stay hand-written in `lib/api.ts` with a comment
saying why. [ADR-0029](../../adr/0029-the-openapi-document-is-generated-to-match-the-wire.md).

**It found a bug in its first minute.** `System.tsx` rendered `version.data.platform` — a field
`GetVersionResponse` has never carried. It had rendered blank since the day it was written, and the
hand-written type it was checked against was written from the same wrong assumption, so nothing
could catch it. One screen, one field, and the only surface that existed. The estate view has four
columns over fifty rows.

## What one row says, and what it deliberately does not

Five facts compete for a row — health, when the last backup was, who took it, whether it was
verified, whether the instance is adherent — and five columns is not a glance.

Four columns. **Origin is not one of them.** For a reader scanning fifty rows origin decides exactly
one thing: whether a verification is possible at all. So it is what the Verified cell *says*, and
the two facts cannot be read apart:

| Backup | Verified cell |
|---|---|
| observed | `n/a — not ours` |
| managed, never verified | `never verified` |
| managed, `FAILED` | **`verification failed`** — the loudest thing on the screen |

A separate Origin column would let someone read Verified alone and misread a blank. That is the one
collapse this screen makes, and it is allowed because it makes the honest reading the only reading
([ADR-0015](../../adr/0015-observed-and-managed-backups.md)).

Adherence and the last backup's time are one cell, because the verdict and its evidence are one
thought. Everything that weakens an answer — B3's caveats, the declared cron and grace — is behind
the row.

**The default order is severity, not name.** A screen sorted alphabetically makes the reader do the
scanning the screen exists to do. Verification failed first, then a missed window, then evidence
that cannot report an outcome, then a managed backup nobody has verified, then an instance nobody
declared anything for, then everything that is fine.

Health is deliberately outside that ranking. A server down right now and backed up correctly last
night is an operational problem; a server up and whose last backup is unrestorable is a data-loss
problem, and this screen is about the second.

### The sub-question that forced a change to core

`Backup.verification` was populated by `GetBackup` and nothing else — one call site,
`service.go:820`. So no list endpoint could render the Verified column, and the options were fifty
round trips per refresh, or populating `Instance.backup_summary`, which the contract has declared
since the schema was written and nothing has ever filled.

Neither. `GetBackupAdherence` now attaches the latest verification to the managed backups it
returns, in one batched `DISTINCT ON` beside the two queries already there. Observed backups are
skipped rather than queried and found empty: "no verification row" means a permanent fact for one
and a gap for the other, and skipping keeps that distinction in the origin where it belongs.

`backup_summary` is marked `deprecated = true` with a comment pointing at `GetBackupAdherence`.
Removing it is breaking; leaving it silent was a lie.

## Health that moves on its own

`discovery` joined `backup` and `observe` as a schedule kind. **No migration** — both CHECK
constraints have permitted it since `000001_init.up.sql`. One `Runner` method, one adapter file the
shape of B3's `observerunner.go`, and one widened condition.

The distinction that decides the job's outcome is worth carrying forward because it is easy to get
backwards: **an instance that is down is a successful probe.** The plugin answered, the answer was
DOWN, inventory recorded it. What fails the job is not being able to ask at all — no plugin, or the
plugin process unreachable — because that is Fleetward's problem rather than the estate's, and a job
that quietly succeeded while learning nothing would let the screen show a stale answer under a fresh
timestamp.

`BackupRunner` became `JobRunner`. It already ran observation, which is not a backup either.

The kind is called `discovery` because that is the name the constraint has carried since the schema
was written. **It probes health; it does not re-run `Discover`.** The name is older than the job.

## And the walk, which found two defects nothing else did

`docker compose up --build`, five instances, and every state the screen has to distinguish. Both
findings were in seams — the same place B3's were.

**A health-probe schedule printed a hint about backup expectations.** `schedule create --kind
discovery` ended with *"nothing was declared about when a backup is due — add `--expect-cron`"*,
which is true of an observe schedule and meaningless for a probe. It sent the reader after a flag
that would do nothing. The hint is now per kind.

**An instance with nothing declared reported a caveat that read like a misconfiguration.**
`GetBackupAdherence` called `evaluateWindow` for every instance including those with no expectation,
and reported the empty string's parse failure as a caveat: *"the declared schedule could not be
evaluated: "" is not a valid cron expression"*. Pre-existing from B3 and invisible until something
rendered caveats — on an estate where most instances have no declaration yet, it is most of the rows
carrying a warning about a schedule nobody wrote. Nothing declared is the ordinary case, and it now
says nothing further.

Then the estate itself, rendered by the real components against the live API rather than a fixture:

```
prod-1  sqlserver · sqlserver:1433   healthy 1m ago     adherent · last 10m ago          verification failed
prod-2  postgresql · postgres:5432   healthy 1m ago     missed · no backup on record     —
prod-3  postgresql · postgres:5432   healthy 1m ago     unproven · last 7m ago           n/a — not ours
prod-4  postgresql · postgres:5432   healthy 1m ago     adherent · last 7m ago           never verified
prod-5  postgresql · 192.0.2.9:5432  down · never reached  nothing declared              —
```

Every claim the slice makes is in those five rows. **`prod-1` is adherent and first**, above the
instance with no backup at all, because its backup succeeded and a restore of it proved it
unrestorable — the artifact was overwritten with 400 KB of `/dev/urandom` and the checksum caught
it. `prod-3` says `n/a — not ours` and never `not verified`. `prod-5` is present rather than hidden,
and its health says `never reached` rather than showing a comfortable blank — a `DOWN` probe
deliberately does not move `last_seen_at`.

The health column moved on its own throughout, from a `discovery` schedule on `* * * * *`. All five
instances went from `UNKNOWN`/`never` to a live answer within seventy seconds of the schedules being
created, with nobody asking.

## What the tests are for

Three things, because a snapshot of a placeholder proves nothing:

1. **Every (origin × verification status) pair**, including the one that must never appear — an
   observed backup rendering as `never verified`, which sends a DBA after a verification that is
   never coming.
2. **That a proven-bad backup is louder than a missing one** — `CLAUDE.md` §5 as an executable
   statement, asserted on `data-tone` rather than a class name, so a restyle that quietened the
   critical state fails.
3. **The default severity order**, over a fixture estate with one row of each state.

The third failed on first run, and the code was right: it ranks an undeclared instance above one
that is fine, and the expectation had them the other way round. Worth recording — the test caught
the test.

## Still open

- **A `verify` job whose verification returned `FAILED` still reads `succeeded` in `job list`.**
  Carried from B2's journal as B4's, and B4 did not fix it. It is a job-state question rather than a
  screen question, and the screen sidesteps it entirely by reading the verification's status and
  never the job's — which is why `prod-1` renders correctly above. The job row is still misleading
  to anyone reading `job list`.
- **The verification carried on the estate view is partial.** Verdict, timing and error message; not
  the per-check results or discrepancies. No column renders them and `GetBackup` has them, but the
  `Verification` on that response is therefore not the whole record.
- **The estate view reports and changes nothing.** No add-instance flow, no schedule management, no
  verification report screen. All still CLI-only, and the fence said so.
- **There is no login, and the UI says nothing about who is looking at it.** Deliberate: every route
  is open, and implying otherwise would be claiming a protection that does not exist. **B6.**
- **`Instance.backup_summary` is deprecated and still declared.** It cannot be removed without a
  breaking change, so it carries a comment pointing at what replaced it.
- **Not virtualized.** Fifty rows do not need it, and ADR-0010's claim that TanStack Table
  virtualizes the grid is inaccurate — virtualization is a separate package that is not installed.
  Recorded so a future session does not add it looking for a promise that was never kept.
- **TanStack Table v9's API is not the v8 API almost everything written about it describes.**
  Features are opt-in and registered as a *type* threaded through the column helper
  (`estate/features.ts`), accessors must return `CellData`, and a row's cells come from
  `getAllCells` rather than `getVisibleCells` unless the visibility feature is registered. Written
  down because it cost the most exploration of anything in this slice.
- **Regenerating `api/openapi/openapi.yaml` on Windows corrupts it invisibly.** The generator embeds
  `.proto` comments as YAML strings, so a CRLF checkout writes literal `\r\n` escapes inside them
  that no line-ending normalization removes and `git diff` does not show. `grep -cF '\r\n'` on the
  result must print 0. Now in `STATUS.md`'s environment notes.
- **Two integration tests fail on this development machine and neither is a regression.** Both are
  in `STATUS.md`'s environment notes and both were reproduced on `main` before B1.
