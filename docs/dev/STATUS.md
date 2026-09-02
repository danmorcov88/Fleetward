# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Slice B1 is complete. Next is B2 — the SQL Server plugin.**

Fleetward now runs without being asked. A schedule declares intent — a cron expression, a timezone,
a verification policy — and the control plane turns it into jobs, leases each one, and records what
happened. A backup interrupted by `kill -9` no longer sits at `running` forever: within one lease
period it is closed as failed, with a message saying why, and the next scheduled run proceeds
normally ([ADR-0025](../adr/0025-an-expired-lease-fails-its-job.md)). Two control planes against one
database are safe by construction, because a job is claimed by a single atomic statement.

B2's brief is not written yet; briefs are written when the slice starts (see
[`slices/README.md`](slices/README.md)). Its content, and the reasons SQL Server is the second
engine rather than the easier MySQL, are in [`../roadmap.md`](../roadmap.md).

Session protocol: [`slices/README.md`](slices/README.md).

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ◐ B1 done, B2 next |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **There is no authentication or authorization.** Every route under `/api/v1/` is open to anyone
  who can reach the port, including the ones that add an instance, create a schedule, and trigger a
  backup. `cfg.Auth` is parsed and validated and read by no file outside `internal/config`. The
  tenant is the constant `metadb.DefaultTenantID`. **B6.**
- **Only PostgreSQL is a real plugin.** MySQL, MongoDB, and Redis handshake and declare no
  capabilities. The claim that a new engine needs no change to core is still untested. **B2.**
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
  The unit suite runs without `-race` there; CI runs it with `-race` on Linux.
- A Windows checkout with `core.autocrlf=true` makes `gofmt` and `buf format --diff` report every
  file in the tree as unformatted. It is a line-ending artefact, not a finding — check a branch in a
  worktree created with `core.autocrlf=false` before believing it.
