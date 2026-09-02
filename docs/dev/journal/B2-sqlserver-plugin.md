# B2 — The SQL Server plugin, and the claim it was written to test

- **Delivered:** 2026-09-02
- **Brief:** [B2-sqlserver-plugin.md](../slices/B2-sqlserver-plugin.md)

`plugins/sqlserver` is the second real engine, and SQL Server itself was never the point. The point
was a sentence that appears in `CLAUDE.md`, in the README, in
[ADR-0003](../../adr/0003-hashicorp-go-plugin.md), and in the shape of the whole conformance suite:
*adding an engine never means modifying core.* One engine cannot test that, because the contract was
written against it. Now two can.

**The result is that the claim holds, and it cost the contract four fields.** None of them is an
escape hatch, none of them names an engine, and each one was found by standing the real image up
rather than by reading its documentation.

## How it was verified

On Windows (amd64), 2026-09-02, against Go 1.25.6, Docker 27.3.1, and
`mcr.microsoft.com/mssql/server:2022-latest` (16.0.4265.3, CU26).

**`go test -tags=conformance ./test/conformance/...` passes in 147 seconds**, and unlike B1 that
sentence means something on this machine. The SQL Server plugin shells out to nothing — `BACKUP` and
`RESTORE` are T-SQL — so it declares no `required_tools`, nothing is missing from `PATH`, and every
Stage 1 case actually runs here rather than skipping:

```
--- PASS: TestBackupRestoreVerify/sqlserver                                    (32.14s)
--- PASS: TestACorruptedArtifactFails/sqlserver                                (29.31s)
--- PASS: TestVerificationFailsWhenTheSourceNoLongerMatchesItsManifest/sqlserver (28.39s)
--- PASS: TestAnUnreachableTargetIsNeverReportedAsDataLoss/sqlserver           (14.59s)
--- PASS: TestAManifestlessBackupIsInconclusive/sqlserver                      (27.80s)
```

The PostgreSQL end-to-end cases skip here for want of `pg_dump`, exactly as they did in B1; CI runs
both engines in full.

**The suite is unchanged.** `git diff main -- test/conformance/` is one new file —
`fixture_sqlserver_test.go`, 106 lines — and the single line in `fixtures_test.go` that registers it.
No assertion, no helper, and no skip condition moved.

**`grep -rniE "sqlserver|mssql|sql server" internal/ web/src/` returns nothing.** That is the
acceptance test for the whole slice, and it is the one worth re-running before believing any future
change to this area.

`golangci-lint run` reports 0 issues, `gofmt -l` is empty, `buf lint`, `buf format --diff`, and
`buf breaking --against main` are clean, `go mod tidy` leaves no drift, and `go run ./tools/docscheck`
passes. All of them were checked in a worktree created with `core.autocrlf=false`, because a Windows
checkout with the default makes every file in the tree look unformatted.

## What the contract was missing

Four fields, all additive, and the interesting thing is that all four are *declarations a plugin
makes*, not conditions core tests for.

### The image refuses a password core is perfectly happy with

Measured, not inferred:

```
ERROR: Unable to set system administrator password: Password validation failed. The password does
not meet SQL Server password policy requirements because it is not complex enough. …
```

The container exits 255 and never becomes ready. Core generates 24 random bytes as base64url
([ADR-0020](../../adr/0020-sandbox-credentials-from-template-placeholders.md)), over an alphabet of
26 uppercase, 26 lowercase, 10 digits, and 2 symbols. Three of the four classes are required, so
failure needs two absent at once, and over 32 characters that is `(52/64)^32` ≈ **0.13 %, about one
sandbox in eight hundred.**

That is the worst frequency a defect can have. Too rare to reproduce, common enough to fire — a
verification that fails for no reason a few times a year, at night, on a machine nobody is watching,
against the one alert this product exists to raise. `SandboxTemplate.password_policy` lets core
satisfy the rule by construction: one character from each required class, the rest random, shuffled.
Not rejection sampling, which would hide the same defect behind a loop with no upper bound.

The test for it asserts the property over 512 generated passwords rather than sampling for the
absence of a failure, because sixteen samples would have found nothing.

### The account the image creates cannot restore anything

The good news was unexpected: `mcr.microsoft.com/mssql/server` supports `MSSQL_USER`,
`MSSQL_PASSWORD`, and `MSSQL_DB`, the direct equivalents of `POSTGRES_USER` and its siblings, so
ADR-0020's placeholder mechanism worked unchanged. Ready in about nine seconds warm, database
created, login working.

Then:

```
SELECT SUSER_NAME(), IS_SRVROLEMEMBER('sysadmin'), IS_SRVROLEMEMBER('dbcreator')
  →      fleetward     0                            0
```

The image grants that login `CONTROL` on its database and nothing at the server level, and SQL
Server documents explicitly that `db_owner` does **not** carry `RESTORE` permission — only
`sysadmin`, `dbcreator`, or the database's actual owner. So the account core generates cannot do the
one thing the sandbox exists for.

`SandboxTemplate.fixed_username` is the fix: the template says `sa`, core renders `{{ .Username }}`
to it and still generates the password, and the readiness probe, the plugin's credentials, and the
fixture's own connection all follow without knowing why. A sandbox lives for minutes on loopback
behind a fresh random password, which is the same trust boundary PostgreSQL already has — its
generated `fleetward` user *is* the cluster superuser.

### There was no way to say "this engine hands over a file"

The decision the slice existed to make, recorded as
[ADR-0026](../../adr/0026-a-shared-directory-carries-a-file-based-artifact.md) after the three
options were drawn out as diagrams in the brief and one was chosen deliberately.

`BACKUP DATABASE … TO DISK` writes a file on the database server's filesystem, where the plugin has
no access at all. `BACKUP TO URL` would have needed the least code and fails on three separate
counts — a static storage credential passing through the least trusted component in the system, a
`CREATE CREDENTIAL` written to a production instance, and an artifact that becomes a visible object
before core ever completes an upload. Co-locating the plugin is correct and is a phase rather than a
slice, and is in any case the same design with both paths equal.

So: a directory the engine and the plugin can both see, under the two names its two users know it
by. Core resolves it, the plugin puts `engine_path` into its statements and opens `local_path`
itself, and the upload from there is the identical code PostgreSQL uses.

### A count that could not be pinned to the artifact

`pg_dump` counts inside the very snapshot it exports, so a PostgreSQL manifest is exact by
construction. `BACKUP DATABASE` is consistent at the LSN it ends on, and a `COUNT(*)` cannot be tied
to that LSN without snapshot isolation or a database snapshot — **both of which are writes to a
monitored instance.**

`ManifestEntry.count_may_have_drifted` is the smallest honest answer. The plugin brackets the backup
and the counting pass with `sys.dm_db_partition_stats`, which is maintained metadata and free to
read; an object nobody wrote to has an exact count and a shortfall on it is data loss, and an object
that changed is flagged and a shortfall on it is drift, reported and `INCONCLUSIVE` rather than
`FAILED`. The polarity is deliberate: the default `false` means exact, so every existing PostgreSQL
manifest keeps its meaning.

The cost is stated in `STATUS.md` rather than hidden: a busy database verifies more weakly than a
quiet one, and the report says so.

## Three things measured that would have cost an afternoon each

**A file SQL Server writes is `-rw-r----- 10001:10001`, and it ignores `umask`.** A `command` wrapper
setting `umask 0022` changes nothing — tried, not assumed. On Linux CI the plugin runs as a different
uid and cannot read its own backup, and on Docker Desktop for Windows the mount layer hides this
completely, so it would have passed locally and failed in CI.

The fix is small and not obvious: **the plugin creates the file, empty, and asks the engine to back
up onto it.** SQL Server opens an existing file rather than creating one, so the owner and mode
survive. `WITH FORMAT, INIT` is what makes it accept a zero-byte file as a fresh media set; without
`FORMAT` it reads the absent header and refuses. The same trick is what lets the restore path put an
artifact where the engine can read it.

**Readiness is not "the port is open".** `MSSQL_DB` is created by a setup pass that runs *after* the
server starts, polling on a five-second loop. A probe of `SELECT 1` against `master` succeeds while
the sandbox database still does not exist — observed directly, as a `Msg 911, Database
'fleetward_sandbox' does not exist`. The readiness command connects to `{{ .Database }}`, which makes
the image's own setup pass the thing being waited for.

**A damaged backup set has its own error numbers, and they are good ones.** A truncated artifact and
a byte-flipped one both give `Msg 3203 … Read on "…" failed: 13(The data is invalid.)`, and a real
restore of the flipped artifact gives `Msg 11801 … RESTORE detected one or more corrupted pages in
the backup set`. Classifying on the number rather than on the text is what keeps ADR-0022's rule
implementable in a second engine: the numbers are stable across versions and languages, and the text
is neither.

## Decisions worth carrying forward

**The upload protocol moved into the SDK rather than being copied.** `ArtifactUploader` and
`FetchArtifact` now live in `internal/plugin/sdk/artifact.go` with their tests. A second copy of
ADR-0021's part-grant upload is a second place for it to drift, and drift there silently truncates a
backup. A second copy of ADR-0022's checksum is a second place to get wrong the one check this
product cannot be wrong about. The PostgreSQL plugin kept only the part that is actually
PostgreSQL's: where it spools the artifact to.

**This plugin orchestrates no external binary, and that turned out to matter twice.** `required_tools`
is empty because `BACKUP` and `RESTORE` are statements. So there is no client package to install, no
`PATH` to get wrong — and no tool exit code for core to misread as evidence about an artifact, which
removes an entire class of the confusion ADR-0022 was written to manage.

**The precondition is checked where a human is asking.** A method declaring
`requires_shared_directory` cannot be run against an instance that has none, and core says so at the
moment someone creates the schedule rather than at 02:00. The message names both halves, because
configuring one of them is the mistake that produces a backup which appears to succeed and then
cannot be read.

**Core changed, and that is not a failure of the claim.** The claim is that core needs no
*engine-specific knowledge* — no name, no lookup table, no branch. `internal/controlplane/sandbox`
learned to honour a fixed username, a password policy, and a mount; `internal/controlplane/backup`
learned to refuse a method whose precondition is unmet. Every one of those reads a field the plugin
published. The acceptance test is the `grep`, and it returns nothing.

## Not built, deliberately

- **Point-in-time recovery and log backups.** `supports_pitr` stays false and `ListPITRTargets`
  answers with an unavailable window and a reason.
- **Always On, failover clusters, replication topology.** `Discover` reports a standalone node.
- **Observed backups from `msdb.dbo.backupset`** — by far the richest such source of any engine, and
  the reason B2 comes before B3. **B3.**
- **A second backup method** — differential, striped, `COPY_ONLY`.
- **`ListPrincipals`, `GetConfig`, `CollectMetrics`.** `principal_model` stays unset.
- **Windows authentication and Kerberos.** SQL authentication only.
- **Multi-database artifacts.** One database per artifact, one artifact per backup.
- **A sweep for backup files left on a real instance's shared directory.** The plugin removes its own
  on every path, including failure; a plugin killed between the two leaks one. Recorded in
  `STATUS.md` rather than fixed here, because it wants the same shape A3 gave leaked containers.

## Still open

- The sandbox image tag is fixed at `2022-latest` rather than derived from the source's version.
  SQL Server's product version and its image tag are different vocabularies — 16.0.x is published as
  "2022-latest" — and mapping between them belongs nowhere in this codebase. A backup restores into
  its own version or a newer one, so the newest exercised tag is safe until a 2025 instance arrives.
- `RESTORE` relocates data and log files and refuses anything else. A database with FILESTREAM or a
  full-text container is answered with a typed `UNSUPPORTED` rather than a wrong guess.
