# ADR-0021: Plugins upload artifacts through presigned multipart parts

- **Status:** Accepted
- **Date:** 2026-08-29
- **Slice:** A4 — backup with a manifest
- **Supersedes:** the single-presigned-`PUT` upload mechanism of
  [ADR-0007](0007-s3-object-storage-for-artifacts.md); that ADR's rule that plugins never hold
  storage credentials is unchanged and is what this mechanism preserves

## Context

ADR-0007 established that a plugin never receives a storage credential: core presigns a grant and
the plugin writes the artifact through it. Slice A4's brief specified the shape that follows from
it — `pg_dump` writes to stdout, and the plugin pipes stdout straight into the body of a presigned
`PUT`, hashing on the way past. Nothing is buffered in memory or on disk.

That does not work, and it does not work for a reason no amount of care in the plugin can fix.

**An S3 `PutObject` requires `Content-Length`.** A body of unknown length forces Go to send
`Transfer-Encoding: chunked`, which the protocol does not permit for this operation. Verified
directly against the MinIO release the development stack runs:

```
PUT chunked status: 411 Length Required
<Error><Code>MissingContentLength</Code>
       <Message>You must provide the Content-Length HTTP header.</Message></Error>
```

The size of a `pg_dump` stream is not known until the dump has finished. So the two requirements —
stream without buffering, and write through a single presigned `PUT` — are mutually exclusive.

Three ways out were considered; they are set out under *Alternatives considered* below.

## Decision

**Artifacts are written as a multipart upload. Core begins and completes it; the plugin writes the
parts through presigned grants and reports their receipts.**

The protocol is:

1. Core calls `CreateMultipartUpload`, which begins the upload and presigns one `PUT` grant per
   part, then puts them in `ArtifactTarget.part_urls` with `part_size_bytes`.
2. The plugin reads the tool's output one part at a time into a single reused buffer, `PUT`s each
   part with an explicit `Content-Length`, and collects the `ETag` the store returns.
3. The plugin reports the receipts in `BackupResult.parts` — a new `UploadedPart` message added to
   the contract for this. It is additively compatible; `buf breaking` passes.
4. Core calls `CompleteMultipartUpload` with the receipts. On any failure it calls
   `AbortMultipartUpload` instead.

Two consequences are load-bearing rather than incidental:

- **Peak memory is one part**, not one artifact. A 500 GB database and a 500 MB one use the same
  memory in the plugin.
- **An incomplete upload is invisible.** Parts that have been written are not an object until the
  upload is completed, so a backup that fails half way cannot leave behind a truncated artifact
  that a later restore would happily load. This is a stronger guarantee than "delete the object if
  the tool failed", which the brief proposed: that has a window in which the object exists.

`ArtifactTarget.upload_url` stays in the contract for a plugin that genuinely knows its artifact's
size before writing it — a snapshot method, say. The PostgreSQL plugin refuses a target without
part grants rather than silently buffering.

## Consequences

**Core has to choose a part count before it knows the size.** It issues 1024 grants, which at the
default 64 MiB part size covers a 64 GiB artifact. A plugin that exhausts them fails with a message
naming the setting to raise, rather than silently truncating the backup. This is a real ceiling and
it is the least comfortable part of this decision; raising `FLEETWARD_OBJSTORE_PART_SIZE_BYTES` is
the answer until something needs a better one.

**The part size has a floor.** S3 rejects any part but the last below 5 MiB, and it rejects it at
*completion* — meaning a part size configured below the floor produces a backup that streams
happily for an hour and then fails at the very end. `NewS3Store` therefore refuses such a
configuration at startup, where it is a one-line fix.

**A plugin cannot retry a part on its own beyond what its HTTP client does.** A part upload that
fails ends the backup, which core reports as retryable. Per-part retry belongs here later; it is
not needed to prove the loop.

**The brief for slice A4 was wrong on this point, and it says so now.** Its "stream; do not buffer"
trap remains correct and is honoured — nothing is buffered beyond one part — but the mechanism it
prescribed could not have worked. The finding is recorded in the brief so a future session does not
try it again.

## Alternatives considered

1. **Spool the artifact to a temporary file, then `PUT` it with a known length.** Simple and
   correct, and it is what several established backup tools do. It also needs local disk equal to
   the compressed size of the database, on the host running the plugin, for every concurrent
   backup. On the estate this product is for, that is not a cost worth paying to avoid a hundred
   lines of code.
2. **Give the plugin storage credentials so it can drive a normal multipart upload.** This is the
   one option that would make the code simplest, and it is the one ADR-0007 exists to forbid. A
   plugin is the least trusted component in the system — third parties will write them — and
   handing it a credential that can read and overwrite every tenant's artifacts to save some
   plumbing is exactly the trade that decision refused.
3. **Multipart upload driven by the plugin through presigned part grants.** Chosen. The contract
   already anticipated it: `ArtifactTarget` carries `part_urls` and `part_size_bytes`, and has
   since the contract was first written.
