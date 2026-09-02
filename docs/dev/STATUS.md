# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice B2 is complete. Next is B3 — observed backups.**

The architecture's central claim is no longer an intention. SQL Server passes the shared conformance
suite — all five end-to-end cases, including the four that are supposed to fail — and the suite is
unchanged: the slice added a fixture beside the plugin and the one line that registers it, and
nothing else under `test/conformance/`. `grep -ri "sqlserver|mssql" internal/ web/src/` returns
nothing.

It cost the contract four fields, all additive, and none of them an engine-specific escape hatch: an
image's fixed administrative account, its password policy, a directory an engine and a plugin can
both see ([ADR-0026](../adr/0026-a-shared-directory-carries-a-file-based-artifact.md)), and a
manifest entry admitting a count it could not pin to the artifact. Each is a declaration core acts on
without learning which engine made it. That is what the slice was for.

Session protocol: [`slices/README.md`](slices/README.md). B3's brief is not written yet; briefs are
written when the slice starts.

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1–B2 done, B3 next |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **There is no authentication or authorization.** Every route under `/api/v1/` is open to anyone
  who can reach the port, including the ones that add an instance, create a schedule, and trigger a
  backup. `cfg.Auth` is parsed and validated and read by no file outside `internal/config`. The
  tenant is the constant `metadb.DefaultTenantID`. **B6.**
- **Five of the eight engines are still binaries that only handshake.** MySQL, MongoDB, and Redis
  declare no capabilities; Oracle, ClickHouse, and Cassandra have no binary at all. PostgreSQL and
  SQL Server are real. **B11–B16.**
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
- **Artifacts accumulate forever, and the scheduler now fills the bucket faster.**
  `backups.expires_at`, `schedules.retention_days`, the `expired` state, and `idx_backups_expiring`
  all exist; `retention_days` is stored on every schedule and read by nothing. **B5.**
- **Nothing is delivered anywhere.** `alert_rules`, `alerts`, and `notifiers` exist in the schema
  and no Go code touches them. A failed verification, and a schedule that has silently stopped
  firing, are both visible only by polling the API. **B7.**
- **Fleetward cannot be observed.** OpenTelemetry is wired in `internal/telemetry/otel.go` with
  zero call sites: no span is started and no meter obtained. There is no `/metrics`. The scheduler
  emits log lines and a readiness component, and nothing else. **B8.**
- **Nothing has been released.** No tag, no published container image, no signed artifact —
  `release.yml` installs cosign and never invokes it. `docker-compose.yml` is a development
  configuration by its own declaration. **B9.**
- **The web UI is a shell.** Two routes; `Estate.tsx` is a placeholder and `lib/api.ts` speaks two
  endpoints with hand-written types. Schedules and jobs have no screen. **B4.**
- **A manually triggered verification is not bounded.** `SCHEDULER_MAX_CONCURRENT_JOBS` bounds
  scheduled work, which is the case that matters on an estate of fifty. A human calling the verify
  endpoint in a loop can still start a sandbox per call.
- **Failed jobs are not retried.** `jobs.max_attempts` exists and nothing decrements against it; a
  failed run waits for its schedule's next occurrence. That is deliberate for now — see the
  alternatives in [ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md) — and a real retry
  policy needs backoff and a window rather than a counter.
- **Only `backup` schedules run.** `schedules.kind` also permits `discovery` and `metrics`; both are
  refused at creation with a message naming the slice that will bring them. **B4.**

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
- **The SQL Server conformance cases run on this machine and the PostgreSQL ones do not.** The
  SQL Server plugin shells out to nothing, so it declares no `required_tools` and nothing is missing;
  PostgreSQL needs `pg_dump`, `pg_restore`, and `psql` on `PATH` and skips without them. Read the
  skip reasons rather than the exit code.
- `mcr.microsoft.com/mssql/server:2022-latest` is 625 MB and becomes ready in about nine seconds
  warm. A full conformance run takes a little under three minutes on this machine with the image
  already pulled.
