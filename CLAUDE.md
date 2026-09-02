# CLAUDE.md — Fleetward project brief

> This file is the durable context for every AI session on this repo. It is derived from the
> Phase 0 + Phase 1 implementation brief. **Read it fully before making changes.** If you make a
> decision that future sessions must respect, record it as an ADR in `docs/adr/` and link it here.

---

## 1. What Fleetward is

Fleetward is an open-source, multi-engine **DBA operations control plane** for an estate of database
servers, unified through a strict plugin contract.

### Who it is for, and the problem it solves

The user is a DBA responsible for roughly fifty servers. They cannot physically check all of them in
a week. The questions they cannot currently answer at a glance are:

- Did every server's backup run on the schedule it was supposed to, and did it succeed?
- Is any of those backups actually restorable?
- Who has access to each database, does their account expire, and is anyone non-compliant?
- Did the schema change in a way nobody intended?

All four have the same shape, and that shape is the product thesis:

> **Declare what should be true → detect what actually is → show me the gap.**

That framing is stronger than "a backup tool with verification", and it is why the three pillars
below belong in one place rather than three tools.

| Pillar | Declared | Detected | Gap surfaced as |
|---|---|---|---|
| **Backup compliance** | schedule, retention | runs observed and managed | missed window, failed run, unverified artifact |
| **Access compliance** | expiry required, least privilege | principals and grants | expired account, no expiry, unexpected superuser |
| **Structural drift** | schema changes only via known migrations | schema snapshots over time | unexplained change |

**Killer feature:** every backup can be automatically restored into an isolated sandbox container
and smoke-tested. Verification status is a first-class, prominently surfaced state — never a
footnote. It is what makes the first pillar trustworthy rather than merely informative.

### Managed and observed backups

Fleetward operates in two modes at once, and the distinction runs through the whole product:

- **Observed** — a backup someone else's cron, script, or tooling took. Fleetward reads the
  evidence and reports compliance. This is what makes adoption on an existing estate possible: a
  tool that demands you migrate fifty servers' backups before showing you anything useful does not
  get adopted.
- **Managed** — a backup Fleetward took. Only these carry a manifest Fleetward captured, and only
  these can be fully verified.

See [ADR-0015](docs/adr/0015-observed-and-managed-backups.md).

### Engines

**Target engines:** PostgreSQL, SQL Server, Oracle, MySQL/MariaDB, MongoDB, ClickHouse, Cassandra,
Redis — eight, all of which exist in the user's real estate, so a multi-engine architecture is a
requirement rather than a thought experiment. **Informix is out of scope.** The list, the order, and
what each one does today live in [`docs/engines.md`](docs/engines.md); do not restate it here.

Adding an engine must never require modifying core. This constraint is the single most important
architectural test: if a change would require core to know an engine's name, the change is wrong.

### Non-goals — do NOT build these

- Our own backup engines. We *orchestrate* native tools (`pg_basebackup`, `xtrabackup`,
  `mongodump`, `BGSAVE`).
- Our own time-series database. We use VictoriaMetrics.
- A Kubernetes operator (later phase).
- BI / analytics features.
- **Writing to database access control.** Fleetward reports non-compliant principals and generates
  the remediation SQL; a human runs it. See [ADR-0017](docs/adr/0017-access-compliance-read-only.md).

A SQL query client was previously a non-goal. It is now the final phase of the roadmap, deliberately
last and gated on conditions recorded in
[ADR-0018](docs/adr/0018-query-editor-on-the-roadmap.md).

### Meta-constraints

- **License:** Apache 2.0. Public GitHub monorepo.
- **Dev machine:** Windows (amd64) as of 2026-09-02; macOS (Apple Silicon) before that. Everything
  must run locally via Docker. CI runs on Linux, plus the unit suite on Windows.
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
| API serving | grpc-gateway handlers registered in-process; **no gRPC listener** until something needs one | [0019](docs/adr/0019-rest-api-without-a-grpc-listener.md) |
| Metadata DB | PostgreSQL 16 (`pgx/v5` with hand-written SQL, `golang-migrate` for migrations) | [0005](docs/adr/0005-postgres-metadata-store.md) |
| Metrics store | VictoriaMetrics single-node; ingest via Prometheus `remote_write`; query via its Prometheus-compatible HTTP API | [0006](docs/adr/0006-victoriametrics-for-metrics.md) |
| Backup artifacts | S3-compatible object storage (MinIO in dev) via `minio-go`, behind our own `ObjectStore` interface | [0007](docs/adr/0007-s3-object-storage-for-artifacts.md) |
| AuthN / AuthZ | OIDC (Dex in dev compose); RBAC roles `viewer < operator < dba < admin`; scope = environment → instance; `tenant_id` on every metadata table from day one | [0008](docs/adr/0008-oidc-rbac-multitenancy.md) |
| Secrets | `SecretsProvider` interface; MVP impl: AES-GCM encrypted-at-rest in Postgres, key from env/file; Vault impl later | [0009](docs/adr/0009-secrets-provider-interface.md) |
| Frontend | React 19 + TypeScript + Vite + Tailwind + shadcn/ui + TanStack (Query/Table/Router) | [0010](docs/adr/0010-react-frontend-stack.md) |
| Internal telemetry | OpenTelemetry Go SDK; DB-facing metrics follow OTel database semantic conventions (`db.client.*`) | [0011](docs/adr/0011-opentelemetry-and-semconv.md) |
| Testing | Unit + `testcontainers-go` integration; one shared **plugin-conformance suite** run against all plugins | [0012](docs/adr/0012-testcontainers-and-conformance-suite.md) |
| Scheduler | Internal cron-style scheduler in control plane (`robfig/cron`), jobs persisted in Postgres, at-most-once via lease locking | [0013](docs/adr/0013-internal-scheduler-with-leases.md) |
| Expired leases | A lease that expires fails its job; the job is never claimed twice, and the next scheduled run creates a new one | [0025](docs/adr/0025-an-expired-lease-fails-its-job.md) |
| Logging | `log/slog`, JSON in prod, pretty in dev | [0014](docs/adr/0014-slog-structured-logging.md) |
| Sandbox credentials | Core generates the identity; the plugin's `SandboxTemplate` places it via `{{ .Username }}` / `{{ .Password }}` / `{{ .Database }}` / `{{ .Port }}` | [0020](docs/adr/0020-sandbox-credentials-from-template-placeholders.md) |
| Artifact transfer | Plugins write artifacts through presigned multipart part grants; core begins and completes the upload, so a partial artifact is never a visible object | [0021](docs/adr/0021-plugins-upload-artifacts-as-multipart-parts.md) |
| Verification outcomes | `FAILED` is reserved for evidence about the artifact; every other failure is `INCONCLUSIVE` | [0022](docs/adr/0022-failed-and-inconclusive-are-different-answers.md) |
| Conformance fixtures | The shared suite carries one per-engine `Fixture` for seeding, and nothing else engine-specific | [0023](docs/adr/0023-conformance-fixtures-seed-what-the-contract-cannot.md) |
| File-based artifacts | An engine that writes its backup to its own filesystem hands it over through a directory the plugin can also see; no credential ever reaches the engine | [0026](docs/adr/0026-a-shared-directory-carries-a-file-based-artifact.md) |
| Observed identity | An observed backup is identified by what the engine calls it; core upserts on that and never parses it | [0027](docs/adr/0027-an-observed-backup-is-identified-by-what-the-engine-calls-it.md) |
| Observation and expectation | Observation is a schedule kind; what a backup should look like is declared, never inferred, and adherence is computed on read | [0028](docs/adr/0028-observation-is-a-schedule-kind-and-an-expectation-is-declared.md) |

### Product and scope decisions

These came later, from understanding who the user is and what their day actually looks like. They
change *what* is built rather than what it is built with.

| Decision | ADR |
|---|---|
| Backups have two origins: observed and managed | [0015](docs/adr/0015-observed-and-managed-backups.md) |
| Structural drift detected via normalized schema snapshots | [0016](docs/adr/0016-schema-drift-snapshots.md) |
| Access compliance is read-only, with generated remediation SQL | [0017](docs/adr/0017-access-compliance-read-only.md) |
| The query editor moves from non-goal to final phase | [0018](docs/adr/0018-query-editor-on-the-roadmap.md) |

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
    plugins/{postgres,sqlserver,mysql,mongodb,redis}/   # thin mains only

  internal/
    config/                          # shared by the server and the CLI
    controlplane/{api,inventory,backup,sandbox,scheduler}/
    plugin/{manager,sdk}/            # sdk = harness plugin authors implement against
    storage/{metadb,tsdb,objstore,secrets}/
      metadb/migrations/             # golang-migrate SQL, go:embed-ed into the binary
      tsdb/prompb/                   # vendored Prometheus remote-write wire format
    telemetry/
    version/

  plugins/{postgres,sqlserver,mysql,mongodb,redis}/   # engine plugin implementations
  web/                                        # React app
  test/{conformance,e2e}/
  deploy/
    docker/                          # Dockerfile, Dockerfile.web
    dev/dex/                         # development IdP configuration
  docs/
    adr/                             # architecture decision records
    dev/{slices,journal}/            # briefs for work ahead, journal for work delivered
```

**Rules:**

- **This tree is what exists, not what is planned.** A directory is added by the slice that fills
  it: `controlplane/{auth,rbac}/` in B6, `deploy/helm/` once the Kubernetes sandbox provider exists.
  Listing a directory before it has contents is how this tree came to describe four packages that
  were never written.
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
| `ListBackupHistory` | Backups Fleetward did not take, read from whatever record the engine keeps |
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
4. **Conformance is the merge gate.** `test/conformance` spins the real engine and asserts the
   contract per plugin: today capabilities and health for every plugin, plus backup →
   restore-to-sandbox → verify for any plugin declaring `supports_sandbox_restore`. Four of those
   end-to-end cases are deliberate failures — a corrupted artifact, a stale manifest, an
   unreachable target — because a verification that has only ever been shown to pass is
   indistinguishable from one that always passes. A plugin merges only when conformance passes.
   This suite is what will later make community-contributed plugins trustworthy.

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

## 6. Roadmap

**The roadmap lives in [`docs/roadmap.md`](docs/roadmap.md), and only there.** It used to be
duplicated here, in `docs/dev/STATUS.md`, in `README.md`, and in `docs/dev/slices/README.md`, and
the four copies had already drifted apart from one another.

What stays here is the shape, because a session needs it before it needs the detail:

- Work is cut into **slices**, not stages. Each slice is independently demoable, carries its own
  operational surface, and ends with `docs/dev/STATUS.md` rewritten and a journal entry added.
- Slices are ordered by when they start paying back, not by architectural tidiness.
- **There is no "production readiness" phase.** Production readiness is a property each slice
  either has or does not ([ADR-0024](docs/adr/0024-production-readiness-is-a-slice-property.md)).
  A capability ships with its enforcement and its limits, in the same slice.
- **Documentation describes what is; briefs describe what will be.** The two never mix in one file.
  Forward-looking behaviour in a reference document is marked `> Planned.` or it is not written.

Current position: [`docs/dev/STATUS.md`](docs/dev/STATUS.md).

---

## 7. Acceptance criteria for the first release

The definition of done for `v0.1.0`, unchanged since the original brief — where it was called
"Phase 1", a name the roadmap no longer uses. The numbering is load-bearing: ADR-0008 and the
journal cite these as §7.3 and §7.5, so criteria are never renumbered.

All must hold:

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

The ambition lives in the **contract and architecture**: a plugin system that carries no engine
knowledge in core, verification-first backups, multi-tenant RBAC designed in from day one.

The discipline lives in the **scope** (§1 non-goals) and in every stage exiting demoable.

**When in doubt: smaller surface, deeper quality** — especially anything touching backup and
restore, where correctness *is* the product.

---

## 10. Working agreements for AI sessions

- Start by reading this file, then `docs/dev/STATUS.md`, then the brief for the slice `STATUS.md`
  says is next, in `docs/dev/slices/`. The session protocol — branch, verify, close out, ship — is
  in [`docs/dev/slices/README.md`](docs/dev/slices/README.md).
- **Respect the slice's scope fence.** Each brief lists what is deliberately *not* in it. A session
  reading the whole roadmap will naturally try to build too much, and a half-finished slice is worse
  than a small complete one.
- Never introduce an engine-name branch in `internal/` or `web/`. Extend `Capabilities` instead.
- Never widen scope into a §1 non-goal, even if it seems like a small addition.
- Run `make lint test` before declaring work complete. If something fails, say so with the output.
- Update `docs/dev/STATUS.md` when a stage's status changes.
- **Keep `README.md` current with every push.** It is the project's front door and the single
  document most readers will ever see. If a change alters what Fleetward does, how it is run, its
  architecture, or its stage, the README changes in the same commit — including its Mermaid
  diagrams. A README that describes a previous version of the project is worse than a short one.
- Prefer adding an ADR over an inline comment for anything a future session might otherwise undo.
