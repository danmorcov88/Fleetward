# Project status

Update this file whenever a slice's status changes. It is the first thing a new session reads
after `CLAUDE.md`.

**Current position: Phase A, slice A4 — backup with manifest.**
Foundation and slices A1, A2, and A3 are complete.

Brief: [`docs/dev/slices/A4-backup-and-manifest.md`](slices/A4-backup-and-manifest.md).
Session protocol: [`docs/dev/slices/README.md`](slices/README.md).

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

Each slice has a self-contained brief in [`docs/dev/slices/`](slices/), written so a session can
start cold from the repository alone. Full rationale for the phase ordering is in `CLAUDE.md` §6.

---

## Phase A — Prove the loop (PostgreSQL) 🔨

The thinnest path through the entire product, using the simplest possible backup method. The point
is to prove the verification loop early, because it is simultaneously the differentiator and the
riskiest piece — everything downstream assumes it works.

| Slice | Content | Demo when done | Status |
|---|---|---|---|
| A1 | PG plugin: real `HealthCheck` + `Discover`, testcontainers integration tests | The plugin connects to a real PostgreSQL 16 and reports version, databases, topology | ✅ |
| A2 | `inventory` service, credential storage via `SecretsProvider`, CLI `instance add\|list\|health` | Add a real server, see it healthy | ✅ |
| A3 | `SandboxProvider` (Docker), teardown guaranteed on every path including panic | A test starts and destroys a container; no orphans survive | ✅ |
| **A4** | Backup via `pg_dump` + `SourceManifest` (per-table row counts) + presigned upload | `backup run` → artifact in MinIO with a manifest | 🔨 next |
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

### A2 — delivered

`internal/controlplane/inventory` implements the whole `InventoryService` contract; the REST API is
live; `fleetward-cli` gained `environment` and `instance` command groups. A real server can be added
and seen healthy from the command line.

Decisions worth carrying forward:

- **No gRPC listener exists** — see [ADR-0019](../adr/0019-rest-api-without-a-grpc-listener.md).
  Services are registered with the generated `RegisterXHandlerServer`, so grpc-gateway calls the
  implementation in-process and the HTTP server is the only listener. `config.GRPC` is still there
  and still unused. The cost is that server-streaming RPCs cannot be served over REST; nothing in
  the control-plane contract needs that yet.
- **REST JSON uses proto field names.** `UseProtoNames` plus `EmitDefaultValues`, so a response reads
  `environment_id` and an empty listing is `{"instances": []}` rather than `{}`. Unknown request
  fields are rejected rather than dropped.
- **Credentials are split, and the split is tested.** Username, database, TLS flags, engine options,
  and the CA certificate live in `connections`; the password and the client *private key* go to the
  `SecretsProvider` as one JSON document under `connection/<connection-uuid>`. The client
  certificate travels with its key, because a half-configured mutual-TLS connection is a worse
  failure than a slightly over-protected certificate.
  `TestStoredPasswordIsCiphertextEverywhere` greps the whole metadata store for the plaintext.
- **`connections.options` holds a structured document**, `{"engine": {...}, "tls": {...}}`, not a flat
  option bag. Fleetward's own fields can then never collide with a key the plugin passes straight
  through to its driver.
- **Environments are required, never created on demand.** An instance's environment is what decides
  whether a destructive operation needs production confirmation, so defaulting one would turn a
  missing field into a safety regression.
- **The port is required too.** Core has no per-engine default port and must not acquire one — that
  is exactly the engine knowledge the plugin contract exists to keep out of core. `Capabilities` has
  no `default_port` field, and adding one is how this would change.
- **`CreateInstance` never probes.** An unreachable server is the kind a user most needs in their
  inventory. Health arrives from `TestConnection`, which caches its result on the row so a
  fifty-server listing renders without fifty probes; `GetInstance` answers from the cached
  `discovery` column and does not touch the monitored database at all.
- **A `DOWN` probe does not move `last_seen_at`.** That column means "the last time we actually
  talked to it", and Phase B's adherence rules will read it that way.
- **Identifiers are validated before they reach a query.** A typo in a URL is the caller's mistake;
  letting PostgreSQL reject the `uuid` cast would turn it into a 500.
- **Listings use keyset pagination** on `(created_at, id)`. An estate is added to while it is being
  read, and an offset would silently skip or repeat rows when that happens.
- **The CLI has no `--password` flag**, on purpose. The password comes from `FLEETWARD_DB_PASSWORD`
  or from `--password-stdin`; a password in argv is visible to every process on the host through
  `ps` and is kept in the shell history of whoever typed it.
- **Only `internal/controlplane/api` decides what a client sees.** The service returns sentinel
  errors, the gRPC layer maps them to status codes, and anything unclassified is logged in full and
  returned as a bare internal error — a pgx or secrets-provider failure can carry a connection
  string.
- **The inventory integration test uses a stub plugin.** The metadata store, the migrations, and the
  AES-GCM provider are real; the engine is not. A1 already proves the PostgreSQL plugin against a
  real server, and core's own tests staying engine-agnostic is the architectural point rather than
  a gap.
- **`delete_artifacts` is accepted and recorded but does nothing yet** — there are no artifacts until
  A4. The flag defaults to false because removing a server from the inventory must not silently
  destroy its backups. Deleting an instance *does* delete its secrets explicitly: `secrets` has no
  foreign key to `connections`, deliberately, so nothing else will ever clean them up.

### A3 — delivered

`internal/controlplane/sandbox` provisions a throwaway database container from a plugin's declared
`SandboxTemplate` and guarantees it is destroyed. `cmd/fleetward` constructs the provider, runs the
orphan sweep at startup, and registers it as a non-critical readiness component. No plugin declares
a template yet, and none needs to until A5 — the integration tests supply their own.

Decisions worth carrying forward:

- **Core generates the sandbox identity; the template places it** —
  see [ADR-0020](../adr/0020-sandbox-credentials-from-template-placeholders.md). This was the one
  place the engine-agnosticism rule was genuinely under pressure: `Credentials()` has to carry a
  username and password, and `SandboxTemplate` has no field for either. Core renders `env`,
  `command`, and `readiness_command` as Go templates against `{{ .Username }}`, `{{ .Password }}`,
  `{{ .Database }}`, and `{{ .Port }}`. Every sandbox therefore gets a distinct password with a
  lifetime of minutes, and nothing is compiled into a plugin binary.
- **The Docker SDK is `github.com/moby/moby/client`, not `github.com/docker/docker/client`.** The
  brief named the latter as "already in `go.sum` via testcontainers"; that stopped being true at
  testcontainers-go v0.43, which moved to the split-out `moby/moby/client` and `moby/moby/api`
  modules. `docker/docker` survives in `go.sum` only as a test dependency of `golang-migrate`, so
  importing it would have added a large module for no reason. The brief's actual decision — the
  Docker SDK directly, not testcontainers, because Ryuk's cleanup is scoped to a test session and a
  control plane is not one — is unchanged. `go.mod` gained no new module, only two promotions from
  indirect to direct.
- **A tag is never guessed.** `ResolveTag` uses `tag_template` when it renders to something usable
  and `default_tag` otherwise, and errors when neither produces a tag. There is deliberately no
  fallback to `latest`: verifying a backup against the wrong engine version is worse than reporting
  that it could not be verified.
- **The image repository is validated, and may not carry its own tag.** `postgres:15` is rejected
  while `registry.internal:5000/db/postgres` is allowed. A repository that smuggles in a tag would
  let a plugin decide which version core believes it verified against.
- **The published port has to be polled for, not read once.** `ContainerStart` returning does not
  mean the port is mapped — Docker Desktop sets its proxy up asynchronously, so an inspect issued
  immediately after start reports an empty binding on the platform this project is developed on.
  This cost an hour; it is the single non-obvious thing in the Docker implementation.
- **Readiness needs two consecutive successes.** A PostgreSQL container runs a temporary server
  during initdb and then restarts it, so one success is not evidence the server answering will
  still be there a second later. The readiness command should also be scoped to TCP (`-h 127.0.0.1`)
  rather than the unix socket, or it will reach the temporary server.
- **Sandboxes bind to loopback when the daemon is local.** A sandbox holds a full copy of a
  production database behind a password that exists for minutes; publishing it on every interface
  by default is not a trade worth making. A remote daemon is the exception, because a loopback
  binding there is unreachable by definition.
- **Sweep is scoped by an owner label**, so a running control plane can sweep without destroying its
  own live sandboxes. That is not enough to make two control planes safe on one Docker daemon: the
  second to start would sweep the first's sandboxes. Such a deployment needs a distinct
  `FLEETWARD_SANDBOX_LABEL_PREFIX` per control plane, and the failure would otherwise look like a
  random verification failure rather than a configuration mistake.
- **The dev stack needed two changes to make verification actually possible in it**, and the
  compose smoke test found both by going `degraded`. The control-plane image runs unprivileged, and
  the Docker socket grants access by group — group 0 inside a Docker Desktop VM, the host's `docker`
  GID on Linux — so `group_add: ["${FLEETWARD_DOCKER_GID:-0}"]` defaults to the macOS answer and CI
  discovers the Linux one with `stat -c '%g'`. And because the control plane is itself a container
  there, a sandbox's published host port is unreachable from it: `FLEETWARD_SANDBOX_NETWORK` puts
  sandboxes on the stack's network to be addressed by container name. When a network is configured
  the provider publishes no host port at all — on a shared network nothing reads it, and a sandbox
  holding a copy of production data should not sit on a host port for free.
- **`TestMain` fails the integration run if a single labelled container survives.** The acceptance
  check in the brief was `docker ps -a --filter "label=fleetward.sandbox"` being empty; a suite that
  passes while leaking has tested the wrong thing, so CI enforces it rather than a human running it.
- **Not built, deliberately:** resource limits, quotas, and any scheduling policy across concurrent
  sandboxes. The scope fence excludes them, and nothing yet runs two verifications at once — but a
  fleet of fifty servers verifying on a schedule will, and `SandboxConfig` has no knob for it today.

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
