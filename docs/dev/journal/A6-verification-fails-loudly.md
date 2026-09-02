# A6 — Proving that verification fails loudly

- **Delivered:** 2026-08-31 ([#52](https://github.com/danmorcov88/Fleetward/pull/52))
- **Brief:** [A6-verification-fails-loudly.md](../slices/A6-verification-fails-loudly.md)

`test/conformance` grew from a capability-and-health suite into one that drives the whole loop
against real engines, and four of its six new cases are failures rather than successes. The
PostgreSQL plugin gained a fix the suite found. Phase A is closed.

Verified on macOS (Apple Silicon, 2026-08-31), two ways.

`make conformance` passes in 89 seconds: every Stage 1 case runs for `postgresql` and skips cleanly
for the three plugins that declare no backup method yet, and no container carrying
`fleetward.sandbox` survives the run.

And the acceptance walk from the brief, end to end against the compose stack. A 66.9 KiB `pg_dump`
of the stack's own PostgreSQL 16.15 came back `VERIFIED` — 19 objects, 12 records. One byte of the
stored object was then flipped in MinIO (offset 34248 of 68497, length unchanged), and
`backup verify` came back:

```
status    FAILED
duration  3.835s
error     the artifact does not match its checksum: 68497 bytes hash to eacbd73d…,
          but e2568685… was recorded when it was written
```

The backup row still reads `SUCCEEDED` beside a `FAILED` verification — the two-part status the UI
will render in B5 — and `docker ps -a --filter "label=fleetward.sandbox"` was empty afterwards.
This is the one path no automated test covers alone: core's own tests prove its reaction to a
corrupt artifact with a stub plugin, and conformance proves the plugin half against a real object.

Decisions worth carrying forward:

- **An unreachable restore target was being reported as a bad backup, and this slice found it.**
  Core reads `ERROR_CODE_TOOL_FAILED` as evidence that the artifact could not be loaded (ADR-0022),
  and `pg_restore` writes `connection to server at "127.0.0.1", port 32770 failed: Connection
  refused` onto the same stderr it writes a broken archive to. Every non-cosmetic diagnostic was
  classified as a tool failure, so a sandbox that died between becoming ready and being restored
  into produced `FAILED` — the product's one critical alert, fired on a container that lost a race.
  The fix is in two halves, because there are two ways to arrive: the plugin confirms the target
  answers before it starts the tool, and `classifyRestoreDiagnostics` gained a third bucket for a
  connection lost mid-run, which outranks whatever wreckage follows it on stderr. Both report
  `ERROR_CODE_CONNECTION_FAILED`, which core already reads as `INCONCLUSIVE`.
- **The connectivity probe runs after the checksum, not before it.** A target that does not answer
  and an artifact that is provably corrupt can both be true at once, and the artifact is the more
  useful answer: it is evidence, and the sandbox is not. The order costs a download on a case that
  was going to fail anyway, which is the cheaper mistake.
- **`RestoreFailureStatus` is exported from `internal/controlplane/backup`** so the conformance
  suite can assert the status core would actually record, rather than a proxy for it. A plugin that
  reports an unreachable sandbox as a tool failure passes every check written against the proxy and
  still fires the alert. It is one pure function and the single place ADR-0022's rule is stated.
- **The suite needs an engine-specific fixture, and that is a designed extension point rather than
  a contract leak.** Fleetward never writes to a monitored instance, so no RPC can create a table —
  and a backup of an empty database proves nothing, because comparing zero objects to zero objects
  succeeds trivially. `Fixture` has two methods, `Seed` and `RemoveRows`; adding an engine means
  registering one, never changing an assertion. The PostgreSQL fixture is under fifty lines and is
  the only engine-specific code in `test/conformance`.
- **The source database is stood up through the sandbox provider, from the plugin's own
  `SandboxTemplate`.** It is not a shortcut: it is the only engine-agnostic way core has to run an
  engine at all, so the suite needs no image name, no port, and no environment of its own. Source
  and target both take the template's `default_tag`, because a restore across major versions fails
  in ways that look exactly like corruption.
- **Corruption happens in the bucket, never through the plugin.** A truncated object and a flipped
  byte are what bit rot and a half-finished upload actually look like; corruption injected through
  the code path under test can only exercise the branch someone remembered to write. Neither case
  needs a tampered manifest, and neither has one.
- **The flipped-byte case is the one that proves the checksum earns its cost.** With the checksum
  comparison disabled, the truncated artifact still failed — `pg_restore` cannot read a cut archive
  — but the flipped one restored to `JOB_PHASE_COMPLETED` and was reported as a successful restore.
  Same length, one byte different, nothing else in the path can see it.
- **The mismatched-counts case uses two real backups.** One manifest is compared against an
  artifact taken after rows were deleted from the source, so every number in the assertion came out
  of the plugin. A5's equivalent test edited a manifest by hand, which proves the comparison works
  but not that it works on evidence.
- **Each case that restores gets its own sandbox; the object store and the Docker provider are
  shared.** A restore populates an instance, so reusing one across cases would make the second case
  depend on the first. MinIO is not what is under test, and starting one per case would triple the
  runtime for no extra evidence.
- **`TestMain` fails the run if a single container carrying `fleetward.sandbox` survives**, the same
  guard the sandbox integration tests use, expressed against the Docker API rather than the CLI. The
  suite runs on every change in CI, so a leak of one container per run degrades the runner quietly
  rather than breaking anything visibly.
- **The conformance job now installs `postgresql-client-16`**, because a Stage 1 case skips without
  the native tooling the plugin orchestrates, and a skipped merge gate is not a merge gate. The
  Makefile timeout went from 30 to 60 minutes for the same reason A5's did.
- **Not built, deliberately:** alerting on a failed verification (B6), the UI treatment that makes
  it visually louder than "no backup yet" (B5), other engines (Phase E), and PITR. The suite covers
  backup, restore and verification; `Discover`, `CollectMetrics` and `ListPrincipals` against a real
  engine are still Stage 0 only, and each becomes a Stage 1 case when the phase that needs it lands.
- **Still open, and now the oldest debt in the tree:** a backup interrupted by `kill -9` stays
  `running` forever (B4 owns it), and nothing bounds concurrent verifications — each holds a
  container and a spooled artifact, and fifty servers verifying on a schedule will need a limit
  `SandboxConfig` has no knob for.

