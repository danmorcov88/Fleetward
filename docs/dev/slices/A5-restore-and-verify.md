# Slice A5 — Restore into a sandbox and verify

## Goal

Restore a backup into a throwaway container and prove it matches the manifest captured when the
backup was taken.

## Why now

This is the product. Everything before it was groundwork; everything after it assumes this loop
works.

## Preconditions

- A3 delivered: `SandboxProvider` provisions and reliably destroys containers.
- A4 delivered: backups exist with a manifest and a checksum.

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

## Files

**New**

- `plugins/postgres/restore.go` — fetch the artifact via presigned URL, verify the checksum, run
  `pg_restore` or `psql` into the target.
- `plugins/postgres/verify.go` — the checks: connectivity, record counts, schema presence.
- `internal/controlplane/backup/verify.go` — orchestration: provision sandbox, call `Restore`, call
  `VerifyRestore`, persist, destroy.
- `cmd/fleetward-cli/verify.go` — `backup verify`.

**Modified**

- `plugins/postgres/plugin.go` — declare `supports_sandbox_restore`, populate `sandbox_template`
  (image `postgres`, tag from the major version, `pg_isready` readiness), and declare the
  verification checks actually implemented.

## Reuse, do not rewrite

| What | Where |
|---|---|
| Sandbox lifecycle | `internal/controlplane/sandbox` from A3 |
| Presigned download | `objstore.ObjectStore.PresignGet` |
| Manifest type | `fwv1.SourceManifest`, produced in A4 |
| Discrepancy reporting | `fwv1.Discrepancy` — already in the contract |
| Capability checks | `sdk.SupportsCheck(caps, check)` |

## Traps

**The sandbox image tag must match the source major version.** Restoring a PostgreSQL 16 dump into
a 15 container fails in ways that look like data corruption. `SandboxTemplate.tag_template` exists
for this; resolve it from the version `Discover` reported.

**`pg_restore` warns constantly and exits non-zero for harmless reasons** — a missing role, an
extension already present. Distinguish fatal from cosmetic, or verification will report failure on
every healthy restore and quickly be ignored.

**Count in the sandbox the same way you counted at the source.** If the manifest counted inside a
transaction and verification counts outside one, the numbers can differ for reasons unrelated to
the backup. Use the same query.

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
