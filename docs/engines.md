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
| **PostgreSQL** | `pg_dump` today; `pg_basebackup` + WAL archiving next, `pgbackrest` as a later method | **Reference plugin** — health, discovery, backup, sandbox restore, verification, and observed backup history implemented |
| **SQL Server** | `BACKUP DATABASE … WITH CHECKSUM`, handed over through a shared directory ([ADR-0026](adr/0026-a-shared-directory-carries-a-file-based-artifact.md)) | **Second engine** — health, discovery, backup, sandbox restore, verification, and observed backup history implemented |
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
  produce the correct verdict. PostgreSQL and SQL Server pass this today.

A plugin that declares it can observe backups it did not take is additionally held to that: a backup
is taken on the instance by the engine's own means, and the plugin has to report it, keep its
identity stable when the same evidence is read again, and honour the watermark the next poll resumes
from. A plugin that declares it cannot is held to refusing the call rather than answering with an
empty list.

Opting into Stage 1 is capability-driven: a plugin needs at least one backup method,
`supports_sandbox_restore`, a `SandboxTemplate`, and a registered fixture. Adding an engine means
adding a fixture beside your plugin; it never means changing an assertion. Full guide:
[writing an engine plugin](dev/writing-an-engine-plugin.md).

## What each engine can see of backups it did not take

Most estates already back themselves up. Fleetward reports on those without changing anything about
them ([ADR-0015](adr/0015-observed-and-managed-backups.md)) — but what it can *prove* about somebody
else's backup depends entirely on what the engine leaves behind, and that varies more between
engines than anything else in this product.

So it is declared rather than assumed. Each plugin publishes what its source can and cannot
establish, and Fleetward reports accordingly.

| Engine | What it reads | Identity survives a rename | Can prove the backup succeeded |
|---|---|---|---|
| **SQL Server** | `msdb.dbo.backupset` — the engine's own record of every backup taken on the instance | yes, `backup_set_uuid` | yes: a row is written only when a backup completes |
| **PostgreSQL** | a configured backup directory, listed | no, the identity is derived from the file name | **no** |
| Every other engine | nothing yet | — | — |

The gap between those two rows is the point, and it is why SQL Server was built before this work
started. PostgreSQL keeps no record of its own backups: `pg_stat_archiver` describes WAL archiving,
which is a different question and says nothing about whether last night's dump ran. A directory can
prove that a file arrived, this big, at this time — and **a truncated dump leaves a file behind
exactly as a complete one does**, so it can never prove success.

Fleetward says so rather than rounding it up. A window satisfied only by a file reads as `unproven`,
never `adherent`, and the answer carries the reason.

> Set the directory with `--backup-dir` and `--backup-dir-local` on `fleetward-cli instance add` —
> the same pair an engine that hands its artifact over as a file uses. Which file names count as
> backups defaults to the usual extensions and can be narrowed with a `backup_file_pattern`
> connection option.

## Engines that hand over a file rather than a stream

Worth stating here because it is what a second engine turned out to cost, and because it decides
what Oracle's RMAN and Cassandra's `nodetool snapshot` will do when they arrive.

`pg_dump` writes to stdout, and the plugin streams that into presigned multipart part grants
([ADR-0021](adr/0021-plugins-upload-artifacts-as-multipart-parts.md)). `BACKUP DATABASE … TO DISK`
writes a file on the *database server's* filesystem, where the plugin has no access at all.

**The artifact is handed over through a directory both of them can see**
([ADR-0026](adr/0026-a-shared-directory-carries-a-file-based-artifact.md)). The instance carries the
path the engine writes to and the path Fleetward reaches the same directory by; the plugin creates
the file, asks the engine to back up onto it, reads it back, uploads it through the same presigned
part grants every other engine uses, and deletes it. No credential of any kind reaches the engine,
and no write reaches its access control.

The two rejected options are recorded in that ADR: `BACKUP TO URL`, which authenticates with a
`CREDENTIAL` object holding a static access key that
[ADR-0007](adr/0007-s3-object-storage-for-artifacts.md) forbids a plugin to handle and which
Fleetward would have to create on a production instance; and co-locating the plugin with the
database server, which is the same design with both paths equal and is what a future agent will use.

> **Operationally this is a precondition, not a preference.** A method that declares
> `requires_shared_directory` cannot be scheduled against an instance that has none, and Fleetward
> says so at the point a human asks rather than at 02:00. Configure it with `--backup-dir` and
> `--backup-dir-local` on `fleetward-cli instance add`.

## Adding an engine

The plugin contract is a public interface. A plugin is a separate binary speaking versioned gRPC
over a local socket, named `fleetward-plugin-<engine type>`, and it can live outside this repository.
Start with [writing an engine plugin](dev/writing-an-engine-plugin.md), which covers the capability
matrix, the build order, the error taxonomy, and what the conformance suite will ask of you.

If making your engine work would require a change inside `internal/`, that is a bug in the contract
rather than a limitation of your plugin — please open an issue.
