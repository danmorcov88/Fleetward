# Project status

Update this file whenever a slice's status changes. It is the first thing a new session reads
after `CLAUDE.md`.

**Current position: Phase A, slice A2 — inventory service and CLI instance commands.**
Foundation and slice A1 are complete.

---

## Foundation ✅

| Deliverable | Status |
|---|---|
| Repository layout per `CLAUDE.md` §3 | done |
| `CLAUDE.md` + ADR-0001..0014 | done |
| buf setup, contract compiled, generated code committed | done |
| `EnginePlugin` contract (10 RPCs, capability matrix) | done |
| Plugin SDK: `Engine`, `Base`, `Serve`, typed errors, capability validation | done |
| Plugin manager: discovery, launch, mTLS handshake, supervision, restart with jittered backoff | done |
| Four plugin binaries handshaking (no engine logic yet) | done |
| Metadata schema v1: 18 tables, every tenant-scoped one carrying `tenant_id` | done |
| Storage layer: metadb, objstore, tsdb, secrets | done |
| AES-GCM secrets provider with envelope encryption | done |
| Control plane: `/healthz`, `/readyz`, `/api/v1/version`, graceful shutdown | done |
| `fleetward-cli`: `version`, `health`, `keygen` | done |
| Web UI shell: app shell, Estate placeholder, live System status | done |
| `docker-compose.yml`: postgres, victoriametrics, minio, dex, control plane, web | done |
| CI: buf lint/breaking/codegen-drift, golangci-lint, tests, build, govulncheck, web, compose smoke | done |
| `Makefile` | done |

**Exit criteria:** `docker compose up` yields a healthy control plane with a green `/readyz`, an
empty UI shell at `localhost:3000`, and green CI. ✅

**Verified on macOS (Apple Silicon), 2026-07-26:** all six services reach healthy, `/readyz`
reports `healthy` across metadb, secrets, objstore, tsdb, and plugins, all four plugin binaries
handshake and reach ready, migrations apply cleanly to version 1, the `audit_log` append-only
trigger rejects both UPDATE and DELETE, and the MinIO bucket is created on first start.

### Notable foundation decisions not in the original brief

- **Go 1.25, not 1.23.** The brief specified Go 1.23+. Current `pgx/v5`, `minio-go/v7`,
  `grpc-gateway/v2`, and the OpenTelemetry SDK all declare `go 1.25`, so 1.25 is the real floor.
  Holding 1.23 would mean pinning older releases of all four, which is a worse trade than raising
  the toolchain. ADR-0002, the Dockerfile, and CI were updated to match.
- **`internal/config`** was added to the layout. Configuration is shared by the server and the CLI,
  so it does not belong under `internal/controlplane/`.
- **Migrations live at `internal/storage/metadb/migrations/`**, not at the repository root, so they
  can be `go:embed`-ed and the control plane can migrate itself with no external files.
- **Prometheus remote-write types are vendored** as a four-message proto under
  `internal/storage/tsdb/prompb/`, rather than depending on `github.com/prometheus/prometheus`,
  whose module graph is enormous relative to what we use. It is excluded from the buf module
  because it is an external wire format we conform to, not part of our published contract.
- **`buf lint` excludes `RPC_RESPONSE_STANDARD_NAME` and `SERVICE_SUFFIX`**, so the contract can use
  the names in the brief (`Capabilities`, `HealthStatus`, `PITRWindow`, `EnginePlugin`). Both
  exclusions are documented inline in `buf.yaml`.
- **Every published port in `docker-compose.yml` is overridable** via a `.env` file (see
  `.env.example`). Developer machines routinely already run a Postgres or a MinIO, and a port
  collision should not be the first thing a new contributor debugs.
- **Container health probes use `127.0.0.1`, not `localhost`.** VictoriaMetrics listens IPv4-only
  while `localhost` resolves to `::1` first in its image, which made a perfectly healthy server
  fail its probe.
- **Plugin capabilities start all false.** Capabilities are a promise core relies on when
  deciding what to do to a production database, so each flag is turned on in the same change that
  implements the behavior behind it — never in advance.

---

## Roadmap — phases and slices

Work is cut into slices, not stages. Each is independently demoable and ends with this file
updated. Development is sporadic, so a session must be able to start without reconstructing context
and finish leaving the tree green. That matters more than speed.

Full rationale for the phase ordering is in `CLAUDE.md` §6.

---

## Phase A — Prove the loop (PostgreSQL) 🔨

The thinnest path through the entire product, using the simplest possible backup method. The point
is to prove the verification loop early, because it is simultaneously the differentiator and the
riskiest piece — everything downstream assumes it works.

| Slice | Content | Demo when done | Status |
|---|---|---|---|
| A1 | PG plugin: real `HealthCheck` + `Discover`, testcontainers integration tests | The plugin connects to a real PostgreSQL 16 and reports version, databases, topology | ✅ |
| **A2** | `inventory` service, credential storage via `SecretsProvider`, CLI `instance add\|list\|health` | Add a real server, see it healthy | 🔨 next |
| A3 | `SandboxProvider` (Docker), teardown guaranteed on every path including panic | A test starts and destroys a container; no orphans survive | ⬜ |
| A4 | Backup via `pg_dump` + `SourceManifest` (per-table row counts) + presigned upload | `backup run` → artifact in MinIO with a manifest | ⬜ |
| A5 | `Restore` into sandbox + `VerifyRestore` (connectivity, record counts) | `backup verify` → `VERIFIED` | ⬜ |
| A6 | Deliberately corrupted artifact; conformance suite grows to cover the path | Corrupted artifact → `FAILED`, with discrepancies listed | ⬜ |

**Exit:** acceptance criterion §7.3 of `CLAUDE.md`, proven for one engine.

### A1 — delivered

`HealthCheck` and `Discover` implemented against a real PostgreSQL, covered by unit tests and by
testcontainers integration tests that now run in CI.

Decisions worth carrying forward:

- **The connection config is built field by field, never as a DSN.** A connection string containing
  a password ends up in error messages, logs, and stack traces; the only reliable prevention is
  never to construct one. `TestConnConfigDoesNotBuildADSN` and
  `TestConnectErrorsNeverLeakThePassword` guard this.
- **An unreachable instance is `HEALTH_STATE_DOWN`, not an RPC error.** "Down" is the most important
  answer this RPC gives, and returning it as a failure would lose the distinction between "the
  database is down" and "we could not ask".
- **Authentication failure is deliberately not retryable.** The same wrong password stays wrong, and
  retrying can trip account lockout on the monitored instance.
- **Missing privileges never fail discovery.** A monitoring account without `pg_read_all_settings`
  or `pg_read_all_stats` is good practice, so `data_directory` and `pg_stat_replication` are
  best-effort; their absence must not turn a permissions choice into a false outage.
- Only three capabilities are declared — `supports_schema_discovery`, `supports_replication`,
  `supports_replication_lag` — and a test asserts the rest stay off until implemented.

---

## Phase B — The compliance console ⬜

Where Fleetward starts solving the actual problem: fifty servers that cannot be checked by hand.

| Slice | Content |
|---|---|
| B1 | ADR-0015 implementation: `ListBackupHistory` RPC, `backups.origin` (`managed` / `observed`) |
| B2 | PG: read existing backup evidence (`pg_stat_archiver`, `backup_label`, configured directory) |
| B3 | Expectation model: declared schedule vs observed runs → adherence |
| B4 | Scheduler: lease, heartbeat, and job recovery when the control plane restarts mid-backup |
| B5 | **Estate Overview** — the first real screen; virtualized grid, two-part backup status |
| B6 | Alerts: backup missing, backup failed, verification failed. Webhook + SMTP notifiers |

**Exit:** one screen answers "which of my fifty servers need attention right now".

---

## Phase C — Access compliance ⬜

| Slice | Content |
|---|---|
| C1 | `ListPrincipals` for PostgreSQL; add `created_at` to `Principal` |
| C2 | Policy engine per ADR-0017: no expiry, expired, unexpected superuser, dormant account |
| C3 | Generated remediation SQL (a human runs it) + UI screen |

## Phase D — Structural drift ⬜

| Slice | Content |
|---|---|
| D1 | `GetSchemaSnapshot` RPC per ADR-0016 |
| D2 | Snapshot storage, diffing, timeline |
| D3 | Alert on unexplained change + UI screen |

## Phase E — The remaining engines ⬜

Same conformance suite, unmodified, in the order the estate needs them:
MySQL/MariaDB → MongoDB → Redis → SQL Server → Oracle → ClickHouse.

## Phase F — Production readiness ⬜

RBAC/OIDC enforced on every route, full audit log, metric collection, retention and expiry, signed
releases with SBOM.

## Phase G — Query editor ⬜

Last, deliberately. ADR-0018 records the five conditions that must hold before it starts.

---

## Changes from the original brief

- **Informix removed** from the target engines. SQL Server, Oracle, and ClickHouse are in the user's
  real estate, so the multi-engine architecture is a requirement rather than a thought experiment.
- **Observer mode added** (ADR-0015). Backups on the existing estate are already being taken;
  requiring their migration before showing value would prevent adoption entirely.
- **Metric collection demoted** to Phase F. It was Stage 2 in the brief, but performance monitoring
  was never named as a pain — that need is already met by existing tooling. Up/down health stays
  early because Estate Overview depends on it.
- **The UI moved earlier**, into Phase B. At fifty servers a CLI table stops being enough.
- **The query editor is no longer a non-goal** (ADR-0018), but it is the final phase and gated.
