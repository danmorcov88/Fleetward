# A5 — Restoring into a sandbox, and proving the restore

- **Delivered:** 2026-08-31 ([#51](https://github.com/danmorcov88/Fleetward/pull/51))
- **Brief:** [A5-restore-and-verify.md](../slices/A5-restore-and-verify.md)

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
  see [ADR-0022](../../adr/0022-failed-and-inconclusive-are-different-answers.md). This is the decision
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

