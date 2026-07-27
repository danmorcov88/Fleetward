# Slice A4 — Backup with a manifest

## Goal

Run a `pg_dump` backup of a real instance, upload the artifact to object storage, and capture a
manifest of what the source contained at the moment it was taken.

## Why now

The manifest is the reason verification can mean anything. Without a record of what the source held,
a restore can only be checked for "did the server start" — and shipping that under the same green
checkmark as a real check is worse than shipping no check at all.

## Preconditions

- A2 delivered: instances exist, credentials resolve.
- A3 delivered: not used here, but A5 follows immediately and needs it.
- MinIO is running and the bucket is created at startup (already true).

## Design decisions already made

**`pg_dump` first, `pg_basebackup` later.** A logical dump yields exact row counts trivially and
restores into any empty database. A physical backup restores a whole cluster and needs a
version-exact image plus recovery configuration. Both will exist — the contract supports several
methods per engine, and the brief explicitly blesses `mysqldump` as method #1 for MySQL. Starting
with the physical method means debugging two hard things at once.

**The plugin uploads through a presigned URL.** It never receives storage credentials. Core presigns
and passes the grant; this is the rule in ADR-0007 and the reason `ArtifactTarget` exists.

**Checksum is computed while streaming**, not by re-reading the artifact afterwards. Reading back a
multi-gigabyte object to hash it doubles the transfer for no benefit.

**The final streamed message is terminal.** Either phase `JOB_PHASE_COMPLETED` carrying a
`BackupResult`, or `JOB_PHASE_FAILED` carrying a `PluginError`. Returning without one leaves core
unable to distinguish success from a crashed stream. Conformance checks this in A6.

## Files

**New**

- `plugins/postgres/backup.go` — the `pg_dump` method: run the tool, stream to the presigned URL,
  build the manifest.
- `plugins/postgres/manifest.go` — per-table row counts.
- `plugins/postgres/backup_test.go` — unit tests for argument construction and manifest shaping.
- `internal/controlplane/backup/service.go` — orchestration: create the row, presign, call the
  plugin, consume the progress stream, persist the result.
- `cmd/fleetward-cli/backup.go` — `backup run`.

**Modified**

- `plugins/postgres/plugin.go` — declare `supports_online_backup`, and the `pg_dump` backup method
  with `required_tools: ["pg_dump"]`.
- `api/proto/.../controlplane.proto` — nothing. `RunBackup` is already defined.

## Reuse, do not rewrite

| What | Where |
|---|---|
| Artifact key convention | `objstore.ArtifactKey(tenant, instance, backup, filename)` |
| Presigning | `objstore.ObjectStore.PresignPut` |
| Redacting a URL for logs | `objstore.SafeURL` — a presigned URL is a bearer credential |
| Typed plugin errors | `sdk.ToolNotFound`, `sdk.ToolFailed`, `sdk.ObjectStoreFailed` |
| Connecting to the source | `plugins/postgres/conn.go` — `connect(ctx, creds)` |
| Credential resolution | the inventory service from A2 |

## Traps

**`pg_dump` must never receive the password on its command line.** Anything on `argv` is visible in
`ps` to every user on the host. Pass it through the `PGPASSWORD` environment variable of the child
process, or a `.pgpass` file created with mode 0600 and removed afterwards.

**Row counts are expensive.** `SELECT count(*)` on a large table is a sequential scan. For a first
implementation, count exactly and set `is_sampled = false`; note in the code where sampling would go
and what `is_sampled = true` would then mean for verification.

**Take counts from a consistent point.** Counting after the dump completes can disagree with the
dump's contents if the database is live. Either count inside a repeatable-read transaction that also
snapshots the dump, or record clearly that the manifest is approximate for a live source. Say which
in the code — a manifest whose accuracy is undocumented is a trap for slice A5.

**Stream; do not buffer.** The artifact must not be held in memory or written to a temporary file
in full. `pg_dump` writes to stdout — pipe it to the HTTP request body.

**A non-zero exit from `pg_dump` still produces output.** A partial artifact uploaded as success is
a corrupt backup that reports green. Check the exit code before marking the backup complete, and
delete the object if the tool failed.

## Scope fence

Not in this slice: restore, verification, sandboxes, scheduling, retention, `pg_basebackup`, WAL
archiving, PITR, compression tuning, incremental backups.

## Done when

```bash
fleetward-cli backup run --instance prod-1
# streams progress, ends with a backup id

mc ls local/fleetward-backups/tenants/.../backups/<id>/     # the artifact is there
```

```bash
docker compose exec -T postgres psql -U fleetward -d fleetward -c \
  "SELECT state, size_bytes, checksum_value, jsonb_array_length(manifest->'entries') FROM backups;"
# succeeded, non-zero size, a checksum, and a manifest with one entry per table
```

Plus `make lint test test-integration conformance` green.
