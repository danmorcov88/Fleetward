# Slice A2 — Inventory service and CLI instance commands

## Goal

Add a real database server to Fleetward and see its health, from the command line.

## Why now

A1 made the PostgreSQL plugin able to answer questions about a real instance, but nothing can ask
it. This slice builds the path from a user's command to the plugin and back: store an instance,
store its credentials safely, resolve them, call the plugin, return the answer.

Everything after this — backup, verification, compliance — sits on that path.

## Preconditions

- Slice A1 delivered: `plugins/postgres` implements `HealthCheck` and `Discover`.
- The metadata schema already has `environments`, `instances`, `connections`, and `secrets`.
- `internal/storage/secrets` has a working AES-GCM provider.
- The plugin manager already routes by engine type; it needs no changes.

## Design decisions already made

**The CLI talks to the REST API, never to the database.** A CLI with database credentials would
duplicate authorization and put the metadata store's password on every operator's laptop. This
means the slice must also expose the API, not just implement the service behind it.

**Credentials are split.** Non-secret connection fields — username, database, TLS flags, options —
live in the `connections` table. The password and any client key material go to the
`SecretsProvider` under the name `connection/<connection-uuid>`. Nothing else may store them.

**No read path ever returns a credential.** `ConnectionSpec` in the contract is inbound only. A
`GetInstance` response has no field for a password, and it must stay that way.

**Everything carries `tenant_id`.** The MVP runs single-tenant against the seeded default tenant
`00000000-0000-0000-0000-000000000001`, but every query filters on it from the first line written.
Retrofitting tenancy later means auditing every query in the project.

## Files

**New**

- `internal/controlplane/inventory/service.go` — the domain service: create, get, list, delete
  instances and environments; resolve credentials for a connection.
- `internal/controlplane/inventory/grpc.go` — implements `fwv1.InventoryServiceServer` by
  delegating to the service. Keep the gRPC layer thin: translation only, no logic.
- `internal/controlplane/inventory/service_test.go` — table-driven unit tests.
- `internal/controlplane/inventory/integration_test.go` — `//go:build integration`; a real
  PostgreSQL for the metadata store via testcontainers.
- `cmd/fleetward-cli/instance.go` — the `instance` command group.

**Modified**

- `cmd/fleetward/main.go` — construct the inventory service, register the gRPC service, mount the
  grpc-gateway handler onto the existing `api.Server` mux.
- `internal/controlplane/api/server.go` — a way to mount additional handlers. `Mux()` already
  exists for exactly this.
- `cmd/fleetward-cli/main.go` — register the new command group.

## Reuse, do not rewrite

| What | Where |
|---|---|
| Connection pool, migrations, `secrets.Store` | `internal/storage/metadb` — `db.Pool()` gives `*pgxpool.Pool` |
| Secret storage and retrieval | `internal/storage/secrets` — `Provider.Put` / `Get` / `Delete`, `Ref{TenantID, Name}` |
| Default tenant identifier | `metadb.DefaultTenantID` |
| Plugin routing | `manager.Client(engineType)` returns the gRPC client and its capabilities |
| Problem-details error shape | `api.WriteProblem` in `internal/controlplane/api/server.go` |
| Request correlation in logs | `telemetry.WithTenantID`, `telemetry.WithRequestID` |
| Generated service interfaces | `api/gen/fleetward/v1` — `InventoryServiceServer`, `RegisterInventoryServiceHandler` |

The contract's `InventoryService` is already defined with HTTP annotations in
`api/proto/fleetward/v1/controlplane.proto`. Implement what is there; do not add RPCs for this
slice.

## Traps

**The `connections` table has a partial unique index** — `idx_connections_one_default` enforces at
most one default connection per instance. Creating a second connection with `is_default = true`
fails at the database level, which is intended. Handle it rather than letting the constraint
surface as a 500.

**Deleting an instance must delete its secret.** `connections` cascades on instance deletion, but
`secrets` is keyed by `(tenant_id, name)` and has no foreign key to it — deliberately, so the
secret store stays independent of the schema around it. If the secret is not deleted explicitly,
credentials for deleted instances accumulate forever.

**`DeleteInstance` has a `delete_artifacts` flag defaulting to false.** Removing an instance from
the inventory must not silently destroy its backups. Honour the flag; in this slice there are no
artifacts yet, so record the intent and leave a TODO referencing slice A4.

**Instance health must not be computed inside `CreateInstance`.** A slow or unreachable server
would make adding it hang or fail. Store first, then probe; `TestConnection` exists for the
add-instance wizard to check before committing.

**Environments come first.** `instances.environment_id` is `NOT NULL` with `ON DELETE RESTRICT`.
Either require an environment up front or create a default one on first use — decide, and say which
in the code.

## Scope fence

Not in this slice:

- Scheduling anything. No jobs, no leases, no cron. That is B4.
- Metric collection or writing to VictoriaMetrics. That is Phase F.
- Authentication or RBAC enforcement. The API is unauthenticated in development; enforcement is
  Phase F. Do not add half of it here.
- `GetConfig` or `ListPrincipals` on the plugin. Those are Phase C and F.
- Web UI. The Estate Overview is B5.
- Backups of any kind.

If the API works but the CLI is unfinished when the session ends, **stop there and ship it**. The
API is independently verifiable with `curl`, and the CLI is a natural second session. Half a
service plus half a CLI is not.

## Done when

```bash
make dev   # in another terminal

# create an environment and an instance
curl -sS -X POST localhost:8080/api/v1/environments \
  -H 'content-type: application/json' \
  -d '{"name":"production","is_production":true}' | jq

curl -sS -X POST localhost:8080/api/v1/instances \
  -H 'content-type: application/json' \
  -d '{"environment_id":"<id>","name":"prod-1","engine_type":"postgresql",
       "host":"host.docker.internal","port":5432,
       "connection":{"username":"postgres","password":"...","database":"postgres"}}' | jq

# the instance is listed, and no password comes back
curl -sS localhost:8080/api/v1/instances | jq
curl -sS localhost:8080/api/v1/instances | grep -i password && echo "LEAK" || echo "no credential in the read path"

# and through the CLI
fleetward-cli instance list
fleetward-cli instance health prod-1     # → UP, with version and signals
```

Plus:

```bash
make lint test test-integration conformance   # all green
```

And in the metadata database, the stored password is ciphertext:

```bash
docker compose exec -T postgres psql -U fleetward -d fleetward \
  -c "SELECT name, length(ciphertext) FROM secrets;"
# a row exists, and no plaintext password appears anywhere in the table
```
