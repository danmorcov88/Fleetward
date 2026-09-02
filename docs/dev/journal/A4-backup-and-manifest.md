# A4 — Backups with a source manifest, uploaded as multipart parts

- **Delivered:** 2026-08-29 ([#49](https://github.com/danmorcov88/Fleetward/pull/49))
- **Brief:** [A4-backup-and-manifest.md](../slices/A4-backup-and-manifest.md)

`internal/controlplane/backup` orchestrates a run; `plugins/postgres` implements the `pg_dump`
method; `fleetward-cli backup run|show` drives it. Verified end to end on macOS (Apple Silicon,
2026-08-29) against the compose stack: a backup of the stack's own PostgreSQL 16 produced a 66.9 KiB
artifact in MinIO, the object downloaded from the bucket hashes to exactly the recorded SHA-256, and
`pg_restore -l` reads it as a valid custom-format archive of 176 entries. The `backups` row carries
`succeeded`, the size, the checksum, and a manifest with one entry per table.

Decisions worth carrying forward:

- **A single presigned `PUT` cannot take a streamed artifact, and the brief was wrong about that** —
  see [ADR-0021](../../adr/0021-plugins-upload-artifacts-as-multipart-parts.md). An S3 `PutObject`
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

