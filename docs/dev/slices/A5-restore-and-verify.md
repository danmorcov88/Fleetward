# Slice A5 — Restore into a sandbox and verify

## Goal

Restore a backup into a throwaway container and prove it matches the manifest captured when the
backup was taken.

## Why now

This is the product. Everything before it was groundwork; everything after it assumes this loop
works.

## Preconditions

- A3 delivered: `sandbox.Provider` provisions and reliably destroys containers.
- A4 delivered: backups exist with a manifest and a checksum. Specifically, a `succeeded` row in
  `backups` carries `bucket`/`object_key`, `size_bytes`, `checksum_value`, `engine_version`, the
  `manifest` document, and `metadata` — which for the `pg_dump` method holds
  `{"format": "custom"|"plain", "database": "<name>"}`.

## Design decisions already made

**Verification compares against the manifest**, not against a fixed expectation. `VerifyRestore`
receives the `SourceManifest` recorded at backup time and reports discrepancies per object, with
expected and actual values, rather than one boolean.

**`FAILED` and `INCONCLUSIVE` are different answers.** A sandbox that never became ready, an image
that could not be pulled, a missing tool — these are infrastructure problems, not data loss.
Reporting them as `FAILED` trains an operator to ignore the one alert that matters most.

**Teardown is guaranteed on every path**, including panic. See A3.

**The checksum is verified before restoring.** Restoring a corrupted artifact and then discovering
the counts are wrong wastes minutes and reports the wrong cause.

**The restore tool is chosen from the artifact's own metadata, never from core.** A4 records
`metadata["format"]` on the backup precisely so this slice knows whether the artifact is a
`pg_restore` archive or a `psql` script. Core passes the metadata through; the plugin branches on
it. Guessing from the object key will not work — the key's leaf name is `artifact`, deliberately
neutral, because core must not learn engine file formats.

**Verification is asynchronous, for the same reason A4's backup is.** `HTTP_WRITE_TIMEOUT` is 60
seconds and a verification pulls an image and starts a container. `RunVerification` creates the
rows, returns `{verification_id, job_id}`, and the CLI polls `GetVerification` — mirror the pattern
already in `internal/controlplane/backup/service.go` rather than inventing a second one.

## Files

**New**

- `plugins/postgres/restore.go` — fetch the artifact via presigned URL, verify the checksum, run
  `pg_restore` or `psql` into the target.
- `plugins/postgres/verify.go` — the checks: connectivity, record counts, schema presence.
- `internal/controlplane/backup/verify.go` — orchestration: provision sandbox, call `Restore`, call
  `VerifyRestore`, persist, destroy.
- `cmd/fleetward-cli/verify.go` — `backup verify`, registered as a subcommand of the existing
  `backup` group in `cmd/fleetward-cli/backup.go`, not as a new top-level group.

**Modified**

- `plugins/postgres/plugin.go` — declare `supports_sandbox_restore`, populate `sandbox_template`
  (image `postgres`, tag from the major version, `pg_isready` readiness), and declare the
  verification checks actually implemented.
- `internal/controlplane/backup/grpc.go` — `RunVerification` and `GetVerification` currently return
  `Unimplemented` naming this slice. Replace them.
- `plugins/postgres/discover_test.go` — `TestCapabilitiesDeclareOnlyWhatIsImplemented` asserts
  `supports_sandbox_restore` is off and that no verification checks are declared. Move both lines
  into the implemented set, exactly as A4 did for `supports_online_backup`.

## Reuse, do not rewrite

| What | Where |
|---|---|
| Sandbox lifecycle | `sandbox.Provider.Provision(ctx, sandbox.Spec)` from A3; `Sandbox.Credentials()` is ready to hand to `Restore` |
| Presigned download | `objstore.ObjectStore.PresignGet` — a GET has none of the `Content-Length` trouble ADR-0021 describes, so a single presigned URL is correct here |
| **Counting objects in the sandbox** | `listTablesSQL`, `listTables`, `collectManifest` in `plugins/postgres/manifest.go` — same package, already unexported and reusable |
| Artifact coordinates and format | the `backups` row: `bucket`, `object_key`, `checksum_value`, `engine_version`, `metadata` |
| Async run orchestration | `internal/controlplane/backup/service.go` — detached run context, `Close` that waits, job rows, failure recording |
| Manifest type | `fwv1.SourceManifest`, produced in A4 |
| Discrepancy reporting | `fwv1.Discrepancy` — already in the contract |
| Capability checks | `sdk.SupportsCheck(caps, check)` |

## Traps

**The sandbox image tag must match the version that produced the artifact, not the version the
instance runs today.** Restoring a PostgreSQL 16 dump into a 15 container fails in ways that look
like data corruption. `SandboxTemplate.tag_template` exists for this, and `sandbox.Spec` takes an
`EngineVersion` — but its doc comment says "reported by Discover", which is the wrong source here.
An instance can be upgraded between the backup and its verification, while the artifact still
restores as whatever produced it. Use `backups.engine_version`, which A4 records for exactly this.
`Backup` in `controlplane.proto` has no field for it yet, so read it from the row; add the field if
the UI ends up needing it.

**`pg_restore` warns constantly and exits non-zero for harmless reasons** — a missing role, an
extension already present. Distinguish fatal from cosmetic, or verification will report failure on
every healthy restore and quickly be ignored.

**Count in the sandbox with the same code that counted at the source — not merely the same idea.**
A4's `listTablesSQL` excludes partitions in favour of their parent, and skips foreign tables and
materialized views, because pg_dump writes no rows for any of them. A second, independently written
counting query will disagree with the manifest on any database using those features, and the
disagreement surfaces as a verification discrepancy: a false alarm on a perfectly good backup, which
is the single worst thing this slice can ship. `collectManifest` is in the same package. Call it.

The one thing that legitimately differs is the transaction: the source counted inside the exported
snapshot, and a freshly restored sandbox has no concurrent writers, so counting outside one there
is fine.

**Destroy with a context that is not the request context.** Verification usually fails by
cancellation, and a cancelled context cannot clean up. `context.WithoutCancel` plus a fresh timeout.

**An empty manifest must not pass.** If a backup somehow carried no entries, comparing zero objects
to zero objects succeeds trivially and reports `VERIFIED`. Treat a missing or empty manifest as
`INCONCLUSIVE` — the loudest possible way to say "this proves nothing".

## Scope fence

Not in this slice: PITR, restoring into a real instance (sandbox only), integrity checks such as
`amcheck`, scheduling, alerting, the UI, other engines.

## Done when

```bash
fleetward-cli backup verify --backup <id>
# → VERIFIED, with per-check results

docker ps -a --filter "label=fleetward.sandbox"   # empty
```

```bash
docker compose exec -T postgres psql -U fleetward -d fleetward -c \
  "SELECT status, duration_ms, jsonb_array_length(checks) FROM verifications;"
# verified, a real duration, and one row per check run
```

Plus `make lint test test-integration conformance` green.
