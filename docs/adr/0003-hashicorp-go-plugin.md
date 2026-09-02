# ADR-0003: `hashicorp/go-plugin` for the engine plugin system

- **Status:** Accepted, with corrections
- **Date:** 2026-07-26

> **Correction, 2026-09-02.** The engine list in the Context below is historical. **Informix is out
> of scope** and no Informix plugin is planned. Cassandra is still targeted. The current list — eight
> engines, their order, and what each does today — is in [`../engines.md`](../engines.md), which is
> the single source. The decision this ADR records is unaffected: it is about the plugin mechanism,
> not about which engines use it.

## Context

The central architectural requirement is that engines beyond the MVP four (SQL Server, Oracle,
Informix, ClickHouse, Cassandra) can be added **without modifying core**. Plugins execute
long-running, resource-heavy, potentially crashing work: they fork `pg_basebackup`, stream
multi-gigabyte artifacts, and link against engine client libraries with conflicting dependencies.

We also intend to accept community-contributed plugins, which means running third-party code.

## Decision

Each engine plugin is a **separate binary**, communicating with the control plane over gRPC on a
local socket, launched and supervised by our plugin manager, using `hashicorp/go-plugin`.

- The manager owns the process lifecycle: launch, handshake, health, restart with exponential
  backoff.
- The contract is `api/proto/fleetward/v1/plugin.proto` and nothing else. Plugins share no Go types
  with core beyond the generated protobuf code and `internal/plugin/sdk`.

## Consequences

- A crashing or leaking plugin cannot take down the control plane; the manager restarts it.
- Plugins can depend on conflicting client libraries without dependency hell in core.
- Community plugins are sandboxable at the process level and can, in principle, be written in any
  gRPC-capable language.
- Cost: serialization overhead on every call, and a more complex local dev loop (plugin binaries
  must be built before the control plane can use them — hence `make build-plugins`).
- Streaming RPCs are mandatory for backup/restore progress; request/response would not survive a
  multi-hour backup.

## Alternatives considered

- **Go `plugin` package (in-process `.so`).** Requires byte-identical toolchain and dependency
  versions, no Windows support, and a plugin panic kills the control plane. Disqualifying.
- **In-tree interface implementations.** Simplest, but violates the core requirement: adding an
  engine would mean modifying and re-releasing core.
- **Plugins as long-running network services.** Heavier operational burden for what is fundamentally
  a local extension mechanism.
