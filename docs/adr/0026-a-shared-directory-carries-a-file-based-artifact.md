# ADR-0026: An engine that writes its artifact to a file hands it over through a shared directory

- **Status:** Accepted
- **Date:** 2026-09-02
- **Slice:** B2 — the SQL Server plugin
- **Relates to:** [ADR-0007](0007-s3-object-storage-for-artifacts.md),
  [ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md),
  [ADR-0020](0020-sandbox-credentials-from-template-placeholders.md),
  [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md)

## Context

Every backup Fleetward has taken so far came out of a pipe. `pg_dump` writes to stdout, the plugin
reads that stream, hashes it on the way past, and writes it into presigned multipart part grants
(ADR-0021). The plugin never holds a storage credential (ADR-0007) and never touches a file, and an
artifact becomes a visible object only when core completes the upload — so a backup that fails half
way cannot leave a truncated artifact behind.

SQL Server does not have a pipe. `BACKUP DATABASE … TO DISK` writes a file on the **database
server's** filesystem, and the plugin has no access to it at all. There is no supported way to make
SQL Server stream a backup to a client: `TO DISK` requires a seekable device, which rules out a
named pipe, and the streaming interface that does exist — VDI — is a Windows COM API that assumes a
process on the database host.

This is not a SQL Server quirk. Oracle's RMAN and Cassandra's `nodetool snapshot` have the same
shape, so whatever is decided here is decided for three of the eight engines on the roadmap. It is
also the last moment it is cheap: `buf breaking` is enforced, but there is no tag, no published
image, and no third-party plugin, so a contract change today costs a regenerate.

Three ways out were available, and they are set out under *Alternatives considered* below.

## Decision

**A plugin whose backup method writes to a filesystem declares that it needs a directory the engine
and the plugin can both see. Core resolves that directory and passes it as two paths.**

Four additive contract changes carry it, and none of them puts an engine's name in core.

```proto
// common.proto — one directory, under the two names its two users know it by.
message SharedDirectory {
  string engine_path = 1;  // the path as the engine sees it, used verbatim in statements
  string local_path  = 2;  // the same directory as the plugin's own process sees it
}
// Credentials is already everything core resolved about reaching this instance for this call.
SharedDirectory shared_directory = 8;

// plugin.proto
message BackupMethod   { bool   requires_shared_directory = 13; }  // so core can refuse, and say why
message SandboxTemplate { string shared_directory        = 10; }   // where core mounts one in a sandbox
```

The protocol on the backup path is then:

1. Core resolves the instance's shared directory and puts both paths in `Credentials`.
2. The plugin **creates the artifact file itself**, empty, at `local_path`.
3. The plugin asks the engine to back up onto that file at `engine_path`.
4. The plugin reads the file back at `local_path`, hashing as it goes, and writes it into the
   presigned part grants — the identical code path a streamed artifact takes.
5. The plugin deletes the file, on every path including failure.

Restore runs the same protocol in reverse: the plugin writes the downloaded artifact into
`local_path`, hashing it in full before a single statement runs against the target (ADR-0022), then
asks the engine to restore from `engine_path`, then deletes.

Step 2 is not bookkeeping, and it is the part that would not have been guessed. SQL Server on Linux
creates a backup file `0640`, owned by the engine's own uid, and ignores `umask` — a plugin running
as any other user cannot read the artifact it just asked for. Opening an **existing** file preserves
that file's owner and mode, so a file the plugin creates stays the plugin's. `WITH FORMAT, INIT` is
what makes SQL Server accept a zero-byte file as a fresh media set.

Two properties are preserved exactly, and they are the reason this option was chosen:

- **No credential of any kind reaches the engine.** Database credentials reach the plugin for one
  call, as before. The storage credential never leaves core, as before.
- **The artifact still becomes visible only when core completes the upload.** Steps 4 and 5 are
  byte for byte the PostgreSQL path; only the source of the stream differs, a file instead of a
  pipe.

## Consequences

**Fleetward gains an operational precondition it must check rather than assume.** An instance whose
plugin declares `requires_shared_directory` and which has no directory configured cannot be backed
up. Core refuses at schedule creation with a message naming what is missing, rather than discovering
it at 02:00. This is a real cost, and it is the honest one: the difficulty of this option is not
code, it is that a human has to configure a directory — which is a failure that can be explained,
unlike a credential nobody can revoke.

**In practice most SQL Server estates already have one**, because a share is where their existing
backups already go. That is also what makes the observed-backup work in B3 land on the same ground.

**Core learns to mount a directory into a sandbox, and nothing else.** `SandboxTemplate` gains a
container path; core creates a host directory, binds it there, destroys it with the sandbox, and
reports both paths in the sandbox's credentials. It does not know what the engine will put in it.

**The plugin owns cleanup, and a crashed plugin leaks a file.** Deletion is deferred on every path,
but a process killed between the backup and the delete leaves an artifact-sized file on the share.
This is the same shape as the leaked-sandbox problem slice A3 solved with a startup sweep, and it is
not solved here: it is recorded in the journal and in `STATUS.md` as known.

**This decision is forward-compatible with a co-located agent rather than an alternative to one.**
An agent running the plugin on the database host is exactly this design with
`engine_path == local_path`. Choosing the shared directory now builds the field that agent will
populate; it does not close the option off.

**It does not generalize to an engine with no filesystem the plugin can reach at all** — a managed
cloud instance, say, where `BACKUP TO URL` is the only egress. That case is real and is not solved
here. When it arrives it needs the operator-provisioned-credential variant under *Alternatives*, and
it needs its own record.

## Alternatives considered

**`BACKUP TO URL` against the S3-compatible store.** SQL Server 2022 speaks S3 natively, and this
would have needed the least code of the three. Rejected on three separate counts, any one of which
is sufficient.

Its authentication is a `CREDENTIAL` object holding a static access key and secret; it cannot
consume a presigned URL. So a storage credential would have to pass through the plugin — the least
trusted component in the system, and the one third parties will write — which is precisely the trade
ADR-0007 refused. Installing that credential means `CREATE CREDENTIAL` on the monitored instance,
making Fleetward write a server-level security object to a production database, which every other
part of this product is built to avoid (ADR-0017, ADR-0018). And the engine writing the object
directly means there is no multipart upload for core to complete, so a half-written backup *is* a
visible object — the failure ADR-0021 exists to make impossible — with the checksum computed after
the fact rather than as the bytes are written.

A variant survives the first two objections: **the operator pre-creates the `CREDENTIAL` and
Fleetward merely names it.** Fleetward would then never see the secret and never write to the
instance. The third objection stands unchanged, and the artifact would land in a bucket whose keys,
retention, and lifecycle Fleetward does not own. It is a reasonable future option for estates where
no share is possible; it is not a foundation to build the second engine on.

**Co-locating the plugin with the database server.** Correct, eventually necessary, and already
anticipated — `CLAUDE.md` §2 names an agent among the things written in Go. It is a phase rather
than a slice: an agent binary, its transport, its supervision, its mTLS, its deployment and its
upgrade story, none of which exist. And, as above, it is a special case of what was chosen rather
than a competitor to it.

**Reading the backup file back over the database connection**, with `OPENROWSET(BULK …,
SINGLE_BLOB)`. Needs no share and no credential anywhere, and was the most tempting of the rejected
options. It fails on memory: the result is a single `varbinary(max)` value, which the driver
materializes whole, so the plugin would hold the entire artifact in RAM — abandoning the one-part
memory ceiling ADR-0021 was written to establish. Chunking it with repeated `SUBSTRING` calls
re-reads the file from the start each time, turning a linear transfer into a quadratic one against a
production server.

**A directory the engine and the plugin can both see.** Chosen.
