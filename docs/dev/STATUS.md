# Project status

The first thing a new session reads after `CLAUDE.md`.

This file is **rewritten**, never appended to. It answers one question — where are we right now —
and everything with a longer lifetime lives elsewhere: rationale in the
[engineering journal](journal/README.md), the plan in [`../roadmap.md`](../roadmap.md), decisions in
[`../adr/`](../adr/), the schema in [`data-model.md`](data-model.md), and every setting in
[`../ops/configuration.md`](../ops/configuration.md). It grew to 586 lines once by ignoring that rule.

---

## Current position

**Phase A is complete. Next is Phase B, slice B1 — the scheduler and the job lease.**

The verification loop is closed and proven in both directions: a backup is taken, restored into a
throwaway container, and compared row for row against the manifest captured with it — and a
deliberately corrupted artifact comes back `FAILED` while a sandbox that never answered comes back
`INCONCLUSIVE`. The whole path is in the shared conformance suite, so every future engine inherits
the proof rather than reinventing it.

B1's brief is not written yet; briefs are written when the slice starts (see
[`slices/README.md`](slices/README.md)). Its content is in [`../roadmap.md`](../roadmap.md): cron
over `schedules`, job claim by lease, heartbeat, and recovery after a crash.

Session protocol: [`slices/README.md`](slices/README.md).

## Phases

| Phase | State |
|---|---|
| Foundation — contract, control plane, dev stack | ✅ [journal](journal/00-foundation.md) |
| A — prove the loop (PostgreSQL), A1–A6 | ✅ [journal](journal/README.md) |
| B — from a proven loop to an installed tool, B1–B16 | ⬜ next |
| Access compliance, structural drift, query editor | deferred — see [roadmap](../roadmap.md#deferred-deliberately) |

There is no Phase F. Production readiness is a property of every slice
([ADR-0024](../adr/0024-production-readiness-is-a-slice-property.md)).

## Known broken, or knowingly absent

Listed so that no session has to re-derive them, and so that no document has to imply otherwise.

- **Nothing runs automatically.** There is no scheduler; every backup and verification is triggered
  by a human. `config.SchedulerConfig` is parsed and cross-validated and read by nothing. **B1.**
- **A backup interrupted by `kill -9` stays `running` forever.** The `jobs` table has
  `lease_owner`, `lease_expires_at`, `heartbeat_at`, and a covering index for the claim query; none
  of them is written. The oldest debt in the tree. **B1.**
- **Nothing bounds concurrent verifications.** Each holds a container and a spooled artifact, and
  fifty servers verifying on a schedule will need a limit `SandboxConfig` has no knob for. **B1.**
- **There is no authentication or authorization.** Every route under `/api/v1/` is open to anyone
  who can reach the port, including the ones that add an instance and trigger a backup. `cfg.Auth`
  is parsed and validated and read by no file outside `internal/config`. The tenant is the constant
  `metadb.DefaultTenantID`. **B6.**
- **Artifacts accumulate forever.** `backups.expires_at`, `schedules.retention_days`, the `expired`
  state, and `idx_backups_expiring` all exist in the schema; nothing writes or reads them. **B5.**
- **Nothing is delivered anywhere.** `alert_rules`, `alerts`, and `notifiers` exist in the schema
  and no Go code touches them. A failed verification is visible only by polling the API. **B7.**
- **Fleetward cannot be observed.** OpenTelemetry is wired in `internal/telemetry/otel.go` with
  zero call sites: no span is started and no meter obtained. There is no `/metrics`. **B8.**
- **Nothing has been released.** No tag, no published container image, no signed artifact —
  `release.yml` installs cosign and never invokes it. `docker-compose.yml` is a development
  configuration by its own declaration. **B9.**
- **Only PostgreSQL is a real plugin.** MySQL, MongoDB, and Redis handshake and declare no
  capabilities. The claim that a new engine needs no change to core is untested. **B2.**
- **The web UI is a shell.** Two routes; `Estate.tsx` is a placeholder and `lib/api.ts` speaks two
  endpoints with hand-written types. **B4.**

## Environment notes

- Verified end to end on macOS (Apple Silicon) through slice A6, and on Windows (amd64) from
  2026-09-02.
- On Windows, plugin binaries must carry `.exe`: `os.Stat` reports no executable bit for any file
  and `exec.Command` resolves through `PATHEXT`. The Makefile appends `GOEXE` for this reason, and
  CI runs the unit suite on `windows-latest` to keep it working.
- `make` is not present on a stock Windows install. The targets can be run directly; a session on
  Windows without it should say so rather than report `make lint test` as passing.
