# CLAUDE.md — Fleetward project brief

> This file is the durable context for every AI session on this repo. It is derived from the
> Phase 0 + Phase 1 implementation brief. **Read it fully before making changes.** If you make a
> decision that future sessions must respect, record it as an ADR in `docs/adr/` and link it here.

---

## 1. What Fleetward is

Fleetward is an open-source, multi-engine **DBA operations control plane**: estate inventory,
health monitoring, **backup with automated restore verification**, access visibility, and alerting —
unified across SQL and NoSQL engines through a strict plugin contract.

**MVP engines:** PostgreSQL, MySQL/MariaDB, MongoDB, Redis.

The architecture must support adding SQL Server, Oracle, Informix, ClickHouse, and Cassandra later
**without modifying core** — only by adding a plugin. This constraint is the single most important
architectural test: if a change would require core to know an engine's name, the change is wrong.

**Killer feature:** every backup can be automatically restored into an isolated sandbox container
and smoke-tested. Verification status is a first-class, prominently surfaced state — never a
footnote.

### Non-goals — do NOT build these

- A SQL query client or GUI.
- Our own backup engines. We *orchestrate* native tools (`pg_basebackup`, `xtrabackup`,
  `mongodump`, `BGSAVE`).
- Our own time-series database. We use VictoriaMetrics.
- A Kubernetes operator (later phase).
- BI / analytics features.
- The five non-MVP engines (they are an architecture constraint, not Phase 1 scope).

### Meta-constraints

- **License:** Apache 2.0. Public GitHub monorepo.
- **Dev machine:** macOS (Apple Silicon). Everything must run locally via Docker. CI runs on Linux.
- **Language:** all code, comments, and docs in English.

---

## 2. Fixed technology decisions — do not relitigate

Each row has a corresponding ADR in `docs/adr/`. Changing one of these requires a new ADR that
supersedes the old.

| Concern | Decision | ADR |
|---|---|---|
| Core language | Go 1.25+ (control plane, agent, all plugins) | [0002](docs/adr/0002-go-as-core-language.md) |
| Plugin architecture | `hashicorp/go-plugin` — each engine plugin is a separate binary, gRPC over local socket, launched and supervised by the plugin manager | [0003](docs/adr/0003-hashicorp-go-plugin.md) |
| API definitions | Protobuf via `buf`; internal gRPC; external REST via `grpc-gateway` + generated OpenAPI v3 | [0004](docs/adr/0004-protobuf-buf-grpc-gateway.md) |
| Metadata DB | PostgreSQL 16 (`pgx/v5`, `sqlc` for queries, `golang-migrate` for migrations) | [0005](docs/adr/0005-postgres-metadata-store.md) |
| Metrics store | VictoriaMetrics single-node; ingest via Prometheus `remote_write`; query via its Prometheus-compatible HTTP API | [0006](docs/adr/0006-victoriametrics-for-metrics.md) |
| Backup artifacts | S3-compatible object storage (MinIO in dev) via `minio-go`, behind our own `ObjectStore` interface | [0007](docs/adr/0007-s3-object-storage-for-artifacts.md) |
| AuthN / AuthZ | OIDC (Dex in dev compose); RBAC roles `viewer < operator < dba < admin`; scope = environment → instance; `tenant_id` on every metadata table from day one | [0008](docs/adr/0008-oidc-rbac-multitenancy.md) |
| Secrets | `SecretsProvider` interface; MVP impl: AES-GCM encrypted-at-rest in Postgres, key from env/file; Vault impl later | [0009](docs/adr/0009-secrets-provider-interface.md) |
| Frontend | React 19 + TypeScript + Vite + Tailwind + shadcn/ui + TanStack (Query/Table/Router) | [0010](docs/adr/0010-react-frontend-stack.md) |
| Internal telemetry | OpenTelemetry Go SDK; DB-facing metrics follow OTel database semantic conventions (`db.client.*`) | [0011](docs/adr/0011-opentelemetry-and-semconv.md) |
| Testing | Unit + `testcontainers-go` integration; one shared **plugin-conformance suite** run against all plugins | [0012](docs/adr/0012-testcontainers-and-conformance-suite.md) |
| Scheduler | Internal cron-style scheduler in control plane (`robfig/cron`), jobs persisted in Postgres, at-most-once via lease locking | [0013](docs/adr/0013-internal-scheduler-with-leases.md) |
| Logging | `log/slog`, JSON in prod, pretty in dev | [0014](docs/adr/0014-slog-structured-logging.md) |

Design tokens and mockups arrive from a **separate design workstream**. Build the UI skeleton
first and restyle when tokens are delivered. Backend work must never block on design.

---

## 3. Repository layout

```
fleetward/
  README.md  LICENSE  CLAUDE.md      # CLAUDE.md is this file
  Makefile                           # make dev / test / lint / proto / build / conformance
  docker-compose.yml                 # full dev stack, one command
  .env.example                       # host port overrides for the dev stack
  buf.yaml  buf.gen.yaml  buf.lock   # protobuf module and codegen (tool-mandated at root)
  .golangci.yml  .goreleaser.yaml    # tool-mandated at root

  .github/
    CONTRIBUTING.md  SECURITY.md  CODE_OF_CONDUCT.md   # GitHub renders these from here
    workflows/                       # ci.yml, release.yml
    ISSUE_TEMPLATE/  PULL_REQUEST_TEMPLATE.md  dependabot.yml

  api/
    proto/fleetward/v1/              # common.proto, plugin.proto, controlplane.proto
    gen/fleetward/v1/                # generated Go (committed, so `go build` needs no buf)
    openapi/                         # generated OpenAPI v3, a release artifact

  cmd/
    fleetward/                       # control-plane server main
    fleetward-cli/                   # CLI (cobra)
    plugins/{postgres,mysql,mongodb,redis}/   # thin mains only

  internal/
    config/                          # shared by the server and the CLI
    controlplane/{api,inventory,scheduler,backup,alerting,rbac,auth}/
    plugin/{manager,sdk}/            # sdk = harness plugin authors implement against
    storage/{metadb,tsdb,objstore,secrets}/
      metadb/migrations/             # golang-migrate SQL, go:embed-ed into the binary
      tsdb/prompb/                   # vendored Prometheus remote-write wire format
    telemetry/
    version/

  plugins/{postgres,mysql,mongodb,redis}/     # engine plugin implementations
  web/                                        # React app
  test/{conformance,e2e}/
  deploy/
    docker/                          # Dockerfile, Dockerfile.web
    dev/dex/                         # development IdP configuration
    helm/                            # placeholder until a later phase
  docs/{adr/,dev/}
```

**Rules:**

- `cmd/plugins/<engine>/` holds only a thin `main` that wires the implementation from
  `plugins/<engine>/`. Keep logic out of `main`.
- The repository root holds only files their tooling requires to be there. Anything with a
  legitimate home elsewhere — community health files, container definitions, generation templates —
  lives in that home.

---

## 4. The plugin contract — the heart of the system

`api/proto/fleetward/v1/plugin.proto` defines `EnginePlugin`. Every engine plugin implements:

| RPC | Purpose |
|---|---|
| `GetCapabilities` | Typed feature matrix |
| `Discover` | Topology, version, databases |
| `GetConfig` | Normalized key/value + raw config |
| `CollectMetrics` | Streams `MetricBatch`, OTel semconv naming |
| `Backup` | Streams progress; writes artifact via object store |
| `Restore` | Streams progress; to sandbox or target |
| `VerifyRestore` | Smoke-tests a restored instance |
| `ListPITRTargets` | Point-in-time-recovery window |
| `ListPrincipals` | Users/roles/privileges, **read-only** |
| `HealthCheck` | Liveness and health signal |

### Non-negotiable rules

1. **Capability-driven, never name-driven.** Core and UI branch *only* on `Capabilities`
   (`supports_pitr`, `supports_schema`, `backup_methods[]`, `principal_model`, …). Grepping for a
   string like `"postgres"` in `internal/` or `web/` is a code smell and almost always a bug.
2. **Plugins never persist credentials.** They receive a `ConnectionRef` per request; core resolves
   it through `SecretsProvider` and passes materialized credentials for that call only.
3. **Plugins orchestrate native tooling** — they do not implement backup formats:
   - PostgreSQL → `pg_basebackup` + WAL archiving. Design the method interface so `pgbackrest`
     can be added as another method.
   - MySQL/MariaDB → `xtrabackup`; `mysqldump` is an acceptable method #1 for MVP simplicity.
   - MongoDB → `mongodump`. Document the path to snapshot-based backups.
   - Redis → RDB snapshot via `BGSAVE` + fetch, with AOF awareness expressed in capabilities.
4. **Conformance is the merge gate.** `test/conformance` spins the real engine via
   `testcontainers-go` and asserts the full contract per plugin: discover → metrics → backup →
   restore-to-sandbox → verify → principals. A plugin merges only when conformance passes. This
   suite is what will later make community-contributed plugins trustworthy.

---

## 5. Backup verification flow — build with extra care

This is the product. Correctness here outranks everything else.

1. Scheduler fires a `BackupJob` → plugin `Backup()` streams progress → artifact lands in the
   object store with a metadata row (instance, method, size, checksum, duration).
2. On success the scheduler enqueues a `VerifyJob` (configurable: always / sampled / manual).
3. `VerifyJob`:
   - Core provisions an **isolated sandbox container** of the matching engine and version (Docker
     API in MVP; abstracted as `SandboxProvider` so Kubernetes Jobs can back it later).
   - Plugin `Restore()` into the sandbox.
   - Plugin `VerifyRestore()` runs smoke checks: connectivity, row/collection/key counts against
     the source manifest, engine-specific integrity checks.
   - Result persisted (`verified` / `failed` + report), metric emitted to VictoriaMetrics, alert
     rule *"backup verification failed"* fires on failure.
   - **Sandbox destroyed always** — guaranteed cleanup via `defer`, including on panic.
4. The UI treats *backup-succeeded-but-verification-failed* as **critical** — visually louder than
   "no backup yet".

---

## 6. Build order

Each stage must end compilable, CI-green, and demoable.

- **Stage 0 — Foundation.** Scaffold §3; CLAUDE.md + ADRs; buf setup + contract compiled; plugin
  manager (launch/handshake/supervise/restart with backoff); metadata schema v1; docker-compose;
  CI (golangci-lint, test, `buf lint`/`buf breaking`, govulncheck, build); Makefile.
  *Exit:* `docker compose up` → healthy control plane, `/readyz` green, empty UI shell, CI green.
- **Stage 1 — PostgreSQL plugin end-to-end.** The reference plugin: full contract incl.
  backup/restore/verify + PITR window; inventory service + REST/OpenAPI; conformance suite born
  here; CLI `instance add/list`, `backup run`, `backup verify`.
  *Exit:* add a Postgres instance via CLI, run a backup, watch verification pass; conformance green.
- **Stage 2 — Metrics & health.** Collection loop (30s default) → `remote_write` to
  VictoriaMetrics; health evaluation (up/down, connections, capability-gated replication lag,
  storage %); events table.
- **Stage 3 — Remaining plugins.** MySQL/MariaDB, MongoDB, Redis against the same conformance
  suite. Expect and fix contract leaks — finding them here, before UI polish, is the point.
- **Stage 4 — Web UI** (overlaps Stage 3). Estate Overview (virtualized grid), Instance Detail
  (capability-adaptive tabs), Backups (list + calendar + PITR timeline + restore wizard with typed
  confirmation), Alerts, OIDC login, Admin users/roles, first-run onboarding.
- **Stage 5 — RBAC/AuthZ + alerting engine.** OIDC end-to-end; middleware enforcing role×scope on
  every route (**server-side**; UI hints only); default alert rules (instance down, verification
  failed, storage >85%, replication lag); webhook + SMTP notifiers; immutable `audit_log` on every
  mutating action.
- **Stage 6 — Release readiness.** CLI polish; README 60-second quickstart; goreleaser
  (darwin/linux × arm64/amd64, signed, SBOM via syft); OpenSSF Scorecard; "writing an engine
  plugin" guide; 5+ `good-first-issue`s; e2e happy path in CI.

**Current stage: see `docs/dev/STATUS.md`.**

---

## 7. Phase 1 acceptance criteria

All must hold before Phase 1 is done:

1. `docker compose up` → UI at `localhost:3000`, login via Dex, all services healthy.
2. A new user adds one instance of **each** of the 4 engines and sees all four healthy on Estate
   Overview with live metrics.
3. A scheduled backup on each engine runs and is **automatically verified**; the UI shows the
   two-part status (backup ✓ / verified ✓); a deliberately corrupted artifact produces
   `verification failed` + a critical alert.
4. A PITR window renders for PostgreSQL (base backup + WAL coverage).
5. `viewer` cannot trigger backup/restore (enforced server-side, 403); `dba` can; every mutating
   action lands in `audit_log`.
6. All 4 plugins pass the conformance suite in CI; unit + integration green; lint clean.
7. Release artifacts: versioned binaries (darwin/linux × arm64/amd64), SBOM, OpenAPI spec
   artifact, `docker compose` quickstart verified on clean macOS (Apple Silicon) and Linux.
8. A developer can scaffold a dummy fifth plugin from `internal/plugin/sdk` + the guide and pass
   the capabilities/health subset of conformance **without touching core**.

---

## 8. Engineering conventions

- **Commits:** Conventional Commits. PR-sized changes. CHANGELOG via release automation. SemVer
  from `v0.1.0`.
- **ADRs:** every §2-level decision has one; new significant decisions get new ADRs.
- **Tests:** table-driven. Integration tests must not require pre-installed engines — testcontainers
  only. Target ≥70% coverage on `internal/`, 100% of the conformance surface per plugin.
- **Security:** TLS-ready config for all listeners; no secrets in logs; parameterized queries only;
  dependency scanning in CI.
- **Errors:** wrap with context (`fmt.Errorf("…: %w", err)`). User-facing API errors use a single
  problem-details JSON shape.
- **Logging:** `slog` with structured fields. Never log a credential, connection string with a
  password, or backup artifact contents.

---

## 9. "Grandios, dar disciplinat"

The ambition lives in the **contract and architecture**: a 9-engine-ready plugin system,
verification-first backups, multi-tenant RBAC from day one.

The discipline lives in the **scope** (§1 non-goals) and in every stage exiting demoable.

**When in doubt: smaller surface, deeper quality** — especially anything touching backup and
restore, where correctness *is* the product.

---

## 10. Working agreements for AI sessions

- Start by reading this file and `docs/dev/STATUS.md`.
- Never introduce an engine-name branch in `internal/` or `web/`. Extend `Capabilities` instead.
- Never widen scope into a §1 non-goal, even if it seems like a small addition.
- Run `make lint test` before declaring work complete. If something fails, say so with the output.
- Update `docs/dev/STATUS.md` when a stage's status changes.
- **Keep `README.md` current with every push.** It is the project's front door and the single
  document most readers will ever see. If a change alters what Fleetward does, how it is run, its
  architecture, or its stage, the README changes in the same commit — including its Mermaid
  diagrams. A README that describes a previous version of the project is worse than a short one.
- Prefer adding an ADR over an inline comment for anything a future session might otherwise undo.
