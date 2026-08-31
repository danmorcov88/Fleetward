# Project status

Update this file whenever a slice's status changes. It is the first thing a new session reads
after `CLAUDE.md`.

**Current position: Phase A, slice A6 — proving verification fails loudly.**
Foundation and slices A1 through A5 are complete. The loop is closed: a backup is taken, restored
into a throwaway container, and compared row for row against the manifest captured with it.

Brief: [`docs/dev/slices/A6-verification-fails-loudly.md`](slices/A6-verification-fails-loudly.md).
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
| A4 | Backup via `pg_dump` + `SourceManifest` (per-table row counts) + presigned upload | `backup run` → artifact in MinIO with a manifest | ✅ |
| A5 | `Restore` into sandbox + `VerifyRestore` (connectivity, record counts) | `backup verify` → `VERIFIED` | ✅ |
| **A6** | Deliberately corrupted artifact; conformance suite grows to cover the path | Corrupted artifact → `FAILED`, with discrepancies listed | 🔨 next |

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

### A4 — delivered

`internal/controlplane/backup` orchestrates a run; `plugins/postgres` implements the `pg_dump`
method; `fleetward-cli backup run|show` drives it. Verified end to end on macOS (Apple Silicon,
2026-08-29) against the compose stack: a backup of the stack's own PostgreSQL 16 produced a 66.9 KiB
artifact in MinIO, the object downloaded from the bucket hashes to exactly the recorded SHA-256, and
`pg_restore -l` reads it as a valid custom-format archive of 176 entries. The `backups` row carries
`succeeded`, the size, the checksum, and a manifest with one entry per table.

Decisions worth carrying forward:

- **A single presigned `PUT` cannot take a streamed artifact, and the brief was wrong about that** —
  see [ADR-0021](../adr/0021-plugins-upload-artifacts-as-multipart-parts.md). An S3 `PutObject`
  requires `Content-Length`; a body of unknown length becomes `Transfer-Encoding: chunked`, which
  MinIO answers with `411 MissingContentLength`. This was verified directly rather than reasoned
  about. Artifacts are therefore written as a **multipart upload**: core begins it and presigns one
  grant per part, the plugin writes parts and returns their ETags in the new `BackupResult.parts`,
  and core completes it. The rule the brief was protecting still holds — nothing is buffered beyond
  one part, so a 500 GB database and a 500 MB one use the same memory.
- **An incomplete upload is invisible, which is stronger than deleting a bad object.** Parts are not
  an object until the upload is completed, so every failure path aborts and no truncated artifact
  ever exists. The brief's "delete the object if the tool failed" has a window; this does not.
- **Core issues 1024 part grants**, covering a 64 GiB artifact at the default 64 MiB part size. It
  cannot know the size in advance, so a bound has to be picked. A plugin that exhausts the grants
  fails with a message naming the setting to raise rather than truncating the backup. This is the
  least comfortable part of the design and the first thing to revisit if a real estate hits it.
- **The part size has a 5 MiB floor and `NewS3Store` refuses anything smaller.** S3 enforces it at
  *completion*, so a smaller value produces a backup that streams happily for an hour and then fails
  at the very end. Refusing at startup turns that into a one-line fix.
- **The manifest is counted inside the same exported snapshot pg_dump reads.** A repeatable-read
  read-only transaction exports a snapshot with `pg_export_snapshot()`, the row counts are taken in
  it, and `pg_dump --snapshot=` is given the same one. Counting against a live database instead
  would make the manifest disagree with its own artifact, and A5 would then report a false
  verification failure on a perfectly good backup — the worst outcome this product can produce.
  `TestManifestCountsComeFromTheExportedSnapshot` commits rows from a second connection at exactly
  the moment between the export and the count, driven by the plugin's own progress message so the
  ordering is guaranteed rather than likely.
- **Partitions are counted through their parent, not individually.** A partitioned parent's count
  already includes every leaf, so counting both would double `total_records`. Foreign tables and
  materialized views are excluded: pg_dump writes no rows for either, so a count taken here could
  never be reproduced in a restored copy.
- **Counts are exact and `is_sampled` stays false.** Sampling is not a faster version of this — it
  is a different verification semantic, where A5 would have to compare within a tolerance. The code
  says where it would go and what it would mean, so it is a decision rather than a silent fallback
  for large tables.
- **`pg_dump` backs up one database, and asking for several is refused.** An empty `databases` list
  means the connection's own database. Silently reducing a multi-database request to the first would
  produce a backup that quietly covered less than was asked for.
- **The child's environment is built from the parent's minus every `PG*` variable.** The control
  plane's own environment may carry `PGDATABASE` or `PGSSLMODE` for unrelated reasons, and
  inheriting one would silently redirect or downgrade a production backup. The password travels in
  `PGPASSWORD`, never on `argv`, and `TestDumpArgsNeverCarryThePassword` guards it.
- **TLS enabled without a CA is refused, not downgraded.** libpq cannot verify a server without a
  root certificate, and `sslmode=require` would encrypt without verifying — a silent downgrade from
  what the operator configured. The message says to supply `ca_pem` or set `insecure_skip_verify`.
  Note this makes `Backup` stricter than `HealthCheck`, which reaches Go's system trust store.
- **`RunBackup` is asynchronous, and it has to be.** `HTTP_WRITE_TIMEOUT` is 60 seconds; a real
  backup is not. The RPC creates the rows and returns `{backup_id, job_id}`, the run continues in
  the control plane, and the CLI polls `GetBackup`. The proto already anticipated this by returning
  a job id.
- **Two concurrent backups of one instance are prevented by a unique index**, not by careful code:
  `idx_jobs_one_active_per_instance_kind` already existed for exactly this, and the second request
  gets `ErrAlreadyRunning`.
- **A backup interrupted by a control-plane restart is still an open problem.** `Service.Close`
  cancels running backups and waits for them to record a failure, so a clean shutdown closes the
  row. A `kill -9` leaves it `running` forever. Slice **B4** owns leases, heartbeats, and restart
  recovery; this is the gap it must close.
- **The runtime image moved from `debian:bookworm-slim` to `debian:trixie-slim`.** bookworm's
  `postgresql-client` is 15, and pg_dump refuses to dump a server newer than itself — it could not
  have backed up the PostgreSQL 16 in our own dev stack. trixie ships 17, covering everything the
  plugin declares (`>=13 <18`).
- **One integration test needs a host tool, and CI installs it.** The plugin orchestrates `pg_dump`
  by design, so a test that mocked it away would prove nothing. `requirePgDump` skips with an
  actionable message when the client is missing or older than the server, and the CI test job
  installs `postgresql-client-16` so the merge gate is real rather than skipped.
- **`SafeURL` moved from `objstore` to `sdk`.** Plugins must redact presigned URLs too, and having a
  plugin import the storage layer would link minio-go into every plugin binary for one string
  function. Both core and plugins already depend on the SDK.
- **`metadb.IsUUID` / `IsUniqueViolation` / `IsForeignKeyViolation` are now shared.** Inventory and
  backup both validate identifiers before they reach a query; each still wraps the failure in its
  own sentinel, because what a client sees is that service's decision.
- **`inventory.ResolveConnection` is the one exported way credentials leave that package.** The
  backup service depends on a `Resolver` interface rather than on `*inventory.Service`, so its tests
  need no secrets provider.
- **Not built, deliberately:** `ListBackups` returns `Unimplemented`. Backup history has to account
  for observed backups and their origin (ADR-0015), and a listing built now would be rebuilt in B1.
  `verify_on_completion` is likewise refused rather than accepted-and-ignored, because promising a
  verification that never happens is precisely this product's worst failure mode. `Backup` in
  `controlplane.proto` has no `engine_version` field, so the column is persisted for A5's use but is
  not yet returned by the API — worth adding when A5 needs it in the UI.
- **Per-part retry does not exist.** A failed part ends the backup, which core reports as retryable.
  It is not needed to prove the loop, but a flaky link on a fifty-server estate will want it.

### A5 — delivered

`plugins/postgres` implements `Restore` and `VerifyRestore`; `internal/controlplane/backup/verify.go`
orchestrates provision → restore → verify → destroy → persist; `fleetward-cli backup verify` drives
it and `backup show` now carries the two-part status. The PostgreSQL plugin declares
`supports_sandbox_restore`, a `sandbox_template`, and the three verification checks it actually
implements.

Verified end to end on macOS (Apple Silicon, 2026-08-31) against the compose stack: a 66.9 KiB
`pg_dump` of the stack's own PostgreSQL 16.15 was restored into a `postgres:16` container the
control plane pulled and provisioned itself, and came back `VERIFIED` in 43s — connectivity, all 19
objects present, 12 rows matching the manifest exactly. `backup run --verify` chained the two in one
command. `docker ps -a --filter "label=fleetward.sandbox"` was empty after both runs, and the
`verifications` row carries `verified`, a real duration, three checks, and `postgres:16` as the
image resolved from the backup's recorded engine version.

Decisions worth carrying forward:

- **`FAILED` is reserved for evidence about the artifact; everything else is `INCONCLUSIVE`** —
  see [ADR-0022](../adr/0022-failed-and-inconclusive-are-different-answers.md). This is the decision
  the whole slice rests on. A verification pulls an image, starts a database, downloads over a
  network the plugin does not control, shells out to a native tool, and only then compares counts;
  of those five steps exactly one says anything about the backup. Reporting the other four as
  `FAILED` would fire the product's one differentiating alert on a slow registry, and an alert that
  fires routinely is muted.
- **A plugin signals "the artifact itself is bad" through a `PluginError` detail**, `artifact:
  corrupt`, constructed by `sdk.ArtifactCorrupt` and read by `sdk.IsArtifactCorrupt`. A checksum
  mismatch and a broken download are discovered in the same place and would otherwise share
  `ERROR_CODE_OBJECT_STORE_FAILED`; telling them apart by string-matching the message is exactly
  the coupling a typed contract exists to prevent. No proto change was needed — `details` is
  already a `map<string, string>`.
- **The artifact is spooled to a private temporary file and hashed in full before the restore tool
  starts.** ADR-0021 refused to pay for local disk on the backup path; here it is worth it, because
  a verification already occupies a whole container and because restoring a corrupted artifact and
  only then noticing the counts are wrong reports the wrong cause. The file is 0600 inside a 0700
  directory and removed on every path, including the failure ones.
- **The restore step is lenient and the count comparison is strict, in that order.** `pg_restore`
  runs with `--no-owner --no-privileges --no-comments` and its remaining diagnostics are classified
  against a short, specific list — a missing role, an object that already exists, an ownership the
  sandbox user does not have — which are waved through and reported in `RestoreResult.metadata`.
  This is only safe because something stricter runs next: a restore that silently dropped a table
  cannot survive a per-table count comparison. A pattern that matched too broadly would waive a real
  failure, so the list is deliberately short rather than convenient.
- **The sandbox image comes from `backups.engine_version`, not from `Discover`.** An instance can be
  upgraded between a backup and its verification while the artifact still restores as whatever wrote
  it, and restoring a PostgreSQL 16 dump into a 15 container fails in ways that look exactly like
  data corruption. The plugin's `tag_template` is `{{ .Major }}`; `default_tag` is a real version
  rather than `latest`, so an old row with no recorded version still cannot be verified against
  something unknown.
- **A manifest-less backup is `INCONCLUSIVE` by construction and never reaches a sandbox.** The
  naive implementation compares zero objects to zero objects, succeeds trivially, and reports
  `VERIFIED` for a backup that proves nothing. Core refuses before provisioning and the plugin
  refuses again if it is ever handed one; both are tested, because the two halves are separately
  implementable.
- **The sandbox is counted with `collectManifest` itself, not with a second query that means the
  same thing.** A4's `listTablesSQL` excludes partitions in favour of their parent and skips foreign
  tables and materialized views, because pg_dump writes no rows for any of them. An independently
  written counting query would disagree with the manifest on any database using those features, and
  the disagreement would surface as a false verification failure — the worst thing this slice could
  ship. The one thing that legitimately differs is the transaction: the source counted inside its
  exported snapshot because it had concurrent writers, and a freshly restored sandbox has none.
- **Schema presence is checked in both directions.** An object present in the restored copy that the
  manifest never recorded means the artifact and its manifest describe different databases, and a
  comparison over only the intersection would call that `VERIFIED`.
- **The job succeeds when a verification reaches a conclusion, including `FAILED`.** The job answers
  "did we manage to check"; the verification answers "was the backup good". Collapsing them would
  make a bad backup indistinguishable from a broken control plane in the job table. Only
  `INCONCLUSIVE` fails the job.
- **`Restore` refuses a `RESTORE_TARGET_KIND_INSTANCE` target with `UNSUPPORTED`.** Restoring over a
  live database needs core's authorization and typed confirmation first; a plugin willing to do it
  before core can gate it is one accident away from an outage, and refusing keeps the capability
  matrix and the behaviour from drifting apart.
- **`verify_on_completion` is now honoured**, which the A5 brief did not list but which the RPC had
  been refusing with a message naming this slice. It chains one verification onto one backup as a
  separate job with its own rows, exposed as `backup run --verify`. The *policy* behind it — always
  / sampled / manual, per instance — is still the scheduler's, and still belongs to B4.
- **`GetBackup` now returns the latest verification** on `Backup.verification`. "Is there a backup"
  and "is it any good" are the same question to whoever is asking, and the field already existed in
  the contract.
- **`backup.New` gained a `sandbox.Provider` parameter**, which may be nil. A control plane with no
  container runtime reports verification as unavailable rather than refusing to serve the estate —
  the same posture readiness already takes for a missing Docker socket.
- **Not built, deliberately:** restoring into a real instance, PITR, `amcheck` and the other
  integrity checks, scheduling, alerting, and the UI. The conformance suite is unchanged; growing it
  to cover restore and verify is A6's, which is also where the corrupted-artifact path gets proven
  end to end rather than through a tampered manifest.
- **Still open from A4, and now more visible:** nothing bounds concurrent verifications. Each holds
  a container and a spooled artifact, and a fifty-server estate verifying on a schedule will need a
  limit that `SandboxConfig` does not have a knob for.

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
