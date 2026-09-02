# A2 — Inventory, credential storage, and the instance CLI

- **Delivered:** 2026-07-30 ([#24](https://github.com/danmorcov88/Fleetward/pull/24))
- **Brief:** [A2-inventory-and-cli.md](../slices/A2-inventory-and-cli.md)

`internal/controlplane/inventory` implements the whole `InventoryService` contract; the REST API is
live; `fleetward-cli` gained `environment` and `instance` command groups. A real server can be added
and seen healthy from the command line.

Decisions worth carrying forward:

- **No gRPC listener exists** — see [ADR-0019](../../adr/0019-rest-api-without-a-grpc-listener.md).
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

