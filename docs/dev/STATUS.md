# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice B3 is complete. Next is B4 — the Estate Overview screen and a generated API client.**

Fleetward now reports on backups it did not take. Point it at an instance, declare when a backup is
supposed to happen, and it answers — with nothing installed on that server, no credential created,
and no backup arrangement migrated:

```
fleetward-cli backup adherence
INSTANCE  ENGINE      EXPECTED   GRACE  LAST BACKUP (UTC)    ADHERENCE
prod-1    sqlserver   0 2 * * *  2h     2026-09-02 02:07:11  adherent
prod-2    postgresql  0 2 * * *  2h     2026-08-24 02:03:55  missed
prod-3    postgresql  —          —      2026-09-02 02:01:02  not_declared
```

That is the first pillar of the product thesis, answerable on the day of installation, for an entire
estate, without owning a single artifact.

The two origins are never rendered as the same thing. A backup Fleetward took carries a manifest and
can be proven restorable; one it observed carries none and never can, and both the API and the CLI
say so rather than leaving a blank. A window satisfied only by evidence that cannot report an
outcome — a file in a directory — reads `unproven`, never `adherent`.

It cost the contract one RPC and six additive fields, and the schema its first migration since the
initial one. Two decisions came out of it:
[ADR-0027](../adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md) on identity,
and [ADR-0028](../adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md) on how
observation runs and where the declaration lives.

Session protocol: [`slices/README.md`](slices/README.md). B4's brief is not written yet; briefs are
written when the slice starts.

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1–B3 done, B4 next |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **There is no authentication or authorization.** Every route under `/api/v1/` is open to anyone
  who can reach the port, including the ones that add an instance, create a schedule, trigger a
  backup, and poll an instance's backup history. `cfg.Auth` is parsed and validated and read by no
  file outside `internal/config`. The tenant is the constant `metadb.DefaultTenantID`. **B6.**
- **Five of the eight engines are still binaries that only handshake.** MySQL, MongoDB, and Redis
  declare no capabilities; Oracle, ClickHouse, and Cassandra have no binary at all. PostgreSQL and
  SQL Server are real. **B11–B16.**
- **A renamed backup file is a second backup.** PostgreSQL's observation reads a directory, which
  assigns no identity, so the plugin derives one from the file name and declares that it did. Core
  reports the caveat on every answer that rests on it rather than inventing a matching heuristic
  ([ADR-0027](../adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md)).
- **An observed backup's finish time is approximate on SQL Server 2019 and older.**
  `CURRENT_TIMEZONE_ID()` arrived in 2022, and it is the only function that returns something
  `AT TIME ZONE` accepts — `CURRENT_TIMEZONE()` returns a display name the engine then rejects. An
  older instance can offer only its current offset, which is wrong by one daylight-saving transition
  for a backup on the other side of one. Those records carry a flag, the compliance window is widened
  by an hour to match, and the answer says so.
- **The observation horizon and overlap are constants rather than configuration.** A first poll of an
  instance reads thirty days back; every poll re-reads six hours before its watermark. Both are named
  and reasoned about in `internal/controlplane/backup/observe.go`. Deliberate, and written down
  because it is a choice rather than an oversight.
- **A backup file left on a shared directory is not swept.** The plugin removes it on every path out
  of a backup or a restore, including failure, but a plugin killed between the two leaks an
  artifact-sized file on the share. A sandbox's own directory is removed with the sandbox; a real
  instance's is not. It has the shape slice A3 solved for containers with a startup sweep, and it
  does not have that sweep.
- **A SQL Server manifest is exact only on a quiescent database.** `BACKUP DATABASE` is consistent at
  the LSN it ends on, and a `COUNT(*)` cannot be tied to that LSN without writing to the monitored
  instance. The plugin brackets the counting pass and flags an object that changed underneath it, and
  a mismatch on a flagged object is `INCONCLUSIVE` rather than `FAILED`. So a busy database verifies
  more weakly than a quiet one, and says so in its report.
- **Artifacts accumulate forever, and nothing expires an observed one on purpose.**
  `backups.expires_at`, `schedules.retention_days`, the `expired` state, and `idx_backups_expiring`
  all exist; `retention_days` is stored on every schedule and read by nothing. An observed backup is
  somebody else's file and Fleetward must never delete it, which is a distinction B5 has to carry.
  **B5.**
- **Nothing is delivered anywhere.** `alert_rules`, `alerts`, and `notifiers` exist in the schema
  and no Go code touches them. A failed verification, a missed backup window, and a schedule that has
  silently stopped firing are all visible only by polling the API. **B7.**
- **Fleetward cannot be observed.** OpenTelemetry is wired in `internal/telemetry/otel.go` with
  zero call sites: no span is started and no meter obtained. There is no `/metrics`. The scheduler
  emits log lines and a readiness component, and nothing else. **B8.**
- **Nothing has been released.** No tag, no published container image, no signed artifact —
  `release.yml` installs cosign and never invokes it. `docker-compose.yml` is a development
  configuration by its own declaration. **B9.**
- **The web UI is a shell.** Two routes; `Estate.tsx` is a placeholder and `lib/api.ts` speaks two
  endpoints with hand-written types. Schedules, jobs, backup history and adherence have no screen.
  **B4.**
- **A manually triggered verification is not bounded.** `SCHEDULER_MAX_CONCURRENT_JOBS` bounds
  scheduled work, which is the case that matters on an estate of fifty. A human calling the verify
  endpoint in a loop can still start a sandbox per call.
- **A job left `running` with no lease cannot be reaped.** That state is by definition an orphan —
  nothing is working on it — and the reaper looks for an *expired lease*, which such a row does not
  have. Nothing produces one today: every job kind now writes its terminal state before releasing
  the lease, and B3's walk found and fixed the one path that did not. Recorded because the reaper's
  blind spot is still there.
- **Failed jobs are not retried.** `jobs.max_attempts` exists and nothing decrements against it; a
  failed run waits for its schedule's next occurrence. That is deliberate for now — see the
  alternatives in [ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md) — and a real retry
  policy needs backoff and a window rather than a counter.
- **Only `backup` and `observe` schedules run.** `schedules.kind` also permits `discovery` and
  `metrics`; both are refused at creation with a message naming the slice that will bring them. **B4.**

## Environment notes

- Verified end to end on macOS (Apple Silicon) through slice A6, and on Windows (amd64) from
  2026-09-02.
- On Windows, plugin binaries must carry `.exe`: `os.Stat` reports no executable bit for any file
  and `exec.Command` resolves through `PATHEXT`. The Makefile appends `GOEXE` for this reason, and
  CI runs the unit suite on `windows-latest` to keep it working.
- `make` is not present on a stock Windows install. The targets can be run directly; a session on
  Windows without it should say so rather than report `make lint test` as passing.
- On Windows, `go test -race` requires cgo and a C toolchain, which a stock install has neither of.
  The unit and conformance suites run without `-race` there; CI runs them with `-race` on Linux.
- A Windows checkout with `core.autocrlf=true` makes `gofmt` and `buf format --diff` report every
  file in the tree as unformatted. It is a line-ending artefact, not a finding — check a branch in a
  worktree created with `core.autocrlf=false` before believing it.
- **Which conformance cases run here depends on what a case needs, not on which engine it is.** The
  SQL Server plugin shells out to nothing, so every case runs for it. PostgreSQL's backup and restore
  cases skip for want of `pg_dump`, `pg_restore` and `psql` on `PATH` — but its **backup-history case
  runs**, because observation shells out to nothing either and the tools gate belongs to the backup
  path alone. Read the skip reasons rather than the exit code.
- `mcr.microsoft.com/mssql/server:2022-latest` is 625 MB and becomes ready in about nine seconds
  warm. A full conformance run takes a little under three minutes on this machine with the image
  already pulled — 167 seconds at B3.
- **Two integration tests fail on this machine for reasons that are the machine's.** Both were
  reproduced on `origin/main` at f0c604f before being blamed on anything.
  `sandbox.TestSandboxLifecycle` connects to its sandbox as `localhost`, which resolves to both
  `127.0.0.1` and `::1`; the sandbox refuses TLS on its mapped port, and the fallback attempt reaches
  the PostgreSQL this machine has installed on `::1:5432` and fails authentication against it.
  `plugins/postgres`'s `TestDiscoverOnUnreachableInstanceFails` dials 192.0.2.1 expecting
  `CONNECTION_FAILED` and gets something pgx reads as `AUTHENTICATION_FAILED` on this network.
  Neither reproduces in CI.
