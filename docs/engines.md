# Supported engines

This page is the single source for which engines Fleetward targets and what each one does today.
The schedule for the ones not yet built is in [`roadmap.md`](roadmap.md).

Fleetward orchestrates native tooling rather than implementing backup formats. Your engine's
maintainers have spent years on those tools; the value is in scheduling, verifying, and reporting on
them.

## Status

<!-- engine-count -->Eight engines<!-- /engine-count -->, drawn from a real estate rather than
chosen for a feature list.

| Engine | Backup method | Status |
|---|---|---|
| **PostgreSQL** | `pg_dump` today; `pg_basebackup` + WAL archiving next, `pgbackrest` as a later method | **Reference plugin** — health, discovery, backup, sandbox restore, and verification implemented |
| **SQL Server** | `BACKUP DATABASE … WITH CHECKSUM`; the transport for the resulting file is an open design question, see below | Slice B2 — the next engine |
| **MySQL / MariaDB** | `mysqldump` first, `xtrabackup` later | Slice B11 — binary handshakes, no capabilities declared |
| **Oracle** | RMAN | Slice B12 |
| **MongoDB** | `mongodump`, snapshot-based to follow | Slice B13 — binary handshakes, no capabilities declared |
| **ClickHouse** | `BACKUP TO Disk`/`S3` | Slice B14 |
| **Cassandra** | `nodetool snapshot` | Slice B15 |
| **Redis** | RDB via `BGSAVE` + fetch, AOF expressed in capabilities | Slice B16 — binary handshakes, no capabilities declared |

A binary that "handshakes" launches, passes the contract-version check, and declares its identity —
and nothing else. Every capability flag is false, because a capability is a promise core relies on
when deciding what to do to a production database, so each is turned on in the same change that
implements the behaviour behind it.

**Informix is out of scope.** It appears in [ADR-0003](adr/0003-hashicorp-go-plugin.md)'s context as
a candidate engine; that mention is historical, and no Informix plugin is planned.

## What "supported" has to mean

An engine is supported when it passes the shared conformance suite — the same suite, unmodified. The
suite has two stages, and only the second one is a real claim:

- **Stage 0** — the binary launches, completes the mTLS handshake, declares a coherent capability
  set, and refuses cleanly what it does not implement. Every plugin in the tree passes this.
- **Stage 1** — a real backup of a real engine is taken, uploaded, restored into a throwaway
  container, and verified against its manifest; then a truncated artifact, a byte-flipped artifact
  of the same length, a manifest that no longer describes its source, and an unreachable target each
  produce the correct verdict. Only PostgreSQL passes this today.

Opting into Stage 1 is capability-driven: a plugin needs at least one backup method,
`supports_sandbox_restore`, a `SandboxTemplate`, and a registered fixture. Adding an engine means
adding a fixture beside your plugin; it never means changing an assertion. Full guide:
[writing an engine plugin](dev/writing-an-engine-plugin.md).

## The question SQL Server asks that PostgreSQL did not

Worth stating here because it is the first real test of the plugin contract, and because it will
shape what every later engine can assume.

`pg_dump` writes to stdout, and the plugin streams that into presigned multipart part grants
([ADR-0021](adr/0021-plugins-upload-artifacts-as-multipart-parts.md)). `BACKUP DATABASE … TO DISK`
writes a file on the *database server's* filesystem, where the plugin has no access at all. Three
ways out, none free:

- a share both sides can see, with the plugin reading and uploading from there;
- `BACKUP TO URL` against an S3-compatible endpoint, which SQL Server 2022 speaks natively — but it
  authenticates with a `CREDENTIAL` object, meaning static storage credentials reach the engine, and
  [ADR-0007](adr/0007-s3-object-storage-for-artifacts.md) says plugins receive presigned URLs and
  never credentials;
- co-locating the plugin with the database server.

The choice gets its own ADR when slice B2 makes it. Oracle's RMAN and Cassandra's `nodetool
snapshot` have the same shape, so whatever is decided here is decided for three engines, not one.

## Adding an engine

The plugin contract is a public interface. A plugin is a separate binary speaking versioned gRPC
over a local socket, named `fleetward-plugin-<engine type>`, and it can live outside this repository.
Start with [writing an engine plugin](dev/writing-an-engine-plugin.md), which covers the capability
matrix, the build order, the error taxonomy, and what the conformance suite will ask of you.

If making your engine work would require a change inside `internal/`, that is a bug in the contract
rather than a limitation of your plugin — please open an issue.
