# Slice A6 — Proving that verification actually verifies

## Goal

Corrupt an artifact deliberately and watch verification report `FAILED` rather than `VERIFIED`.

## Why now

Every check written so far has been tested on the happy path. A verification system that has only
ever been shown to pass is indistinguishable from one that always passes — and the second is far
worse than no verification at all, because it manufactures confidence.

This slice closes Phase A by proving the negative case, and folds the whole loop into the shared
conformance suite so that every future engine inherits the same proof.

## Preconditions

- A5 delivered: the full loop works on the happy path.

## Design decisions already made

**The conformance suite grows to cover backup, restore and verification**, gated on capabilities.
A plugin declaring `supports_sandbox_restore` gets the full path; one that does not is skipped
rather than failed. This is how the suite stays useful from a plugin's first commit while becoming
the merge gate for a complete one (ADR-0012).

**Corruption is tested at more than one layer**, because the failures are different: a truncated
artifact fails at restore, while a subtly altered one restores cleanly and fails on counts. Both
must be caught, and they exercise different code.

## Files

**New**

- `test/conformance/backup_test.go` — the capability-gated backup → restore → verify path.
- `test/conformance/corruption_test.go` — the negative cases.

**Modified**

- `test/conformance/conformance_test.go` — extend the harness with object storage and a sandbox
  provider, which the current one does not need.
- `docs/dev/writing-an-engine-plugin.md` — document what a plugin must do to pass the new checks.

## The cases that must be covered

| Case | Expected |
|---|---|
| Healthy artifact | `VERIFIED` |
| Truncated artifact | `FAILED` — checksum mismatch, detected before restore |
| Artifact with bytes flipped mid-stream | `FAILED` |
| Rows deleted from the source after the manifest was taken | `FAILED`, with a `Discrepancy` naming the table and both counts |
| Sandbox that never becomes ready | `INCONCLUSIVE`, never `FAILED` |
| Missing or empty manifest | `INCONCLUSIVE` |

The last two matter as much as the first four. A system that reports infrastructure trouble as data
loss gets muted, and a muted alert is the same as no alert.

## Traps

**Do not corrupt the artifact through the plugin.** Alter it in object storage, the way real bit
rot or a failed upload would. Corruption injected through the code path under test proves less than
it appears to.

**The conformance suite must clean up its own sandboxes and objects.** It runs in CI on every
change; leaking a container per run degrades the runner quietly.

**Verification runtime is now real.** Pulling an image and restoring takes minutes. Keep the fast
unit path separate so the inner development loop stays quick, and give the conformance job a
generous timeout in CI.

## Scope fence

Not in this slice: alerting on failure (B6), the UI treatment of a failed verification (B5), other
engines (Phase E), PITR.

## Done when

```bash
make conformance
# every case above passes, including all four failure modes
```

A manual walk of the acceptance criterion, end to end:

```bash
fleetward-cli backup run --instance prod-1
fleetward-cli backup verify --backup <id>        # → VERIFIED

# corrupt it where it lives
mc cp local/fleetward-backups/.../artifact.dump /tmp/a && \
  printf 'garbage' | dd of=/tmp/a bs=1 seek=1024 conv=notrunc && \
  mc cp /tmp/a local/fleetward-backups/.../artifact.dump

fleetward-cli backup verify --backup <id>        # → FAILED, with the reason
docker ps -a --filter "label=fleetward.sandbox"  # empty
```

**Phase A is complete when this passes.** Update `STATUS.md`, and update the README — the claim it
makes on the front page is now demonstrably true, which is worth saying plainly.

---

## What the slice found — written down after the fact

Two things the brief could not have known, recorded here because the next engine will meet both.

**The fifth row of the table was not passing, and the design was the reason.** Core reads
`ERROR_CODE_TOOL_FAILED` as evidence that the artifact could not be loaded, and reports it as a
failed verification (ADR-0022). But `pg_restore` writes `connection to server at "127.0.0.1", port
32770 failed: Connection refused` to the same stderr it writes a broken archive to, and the plugin
classified every non-cosmetic diagnostic as a tool failure. A sandbox that died between becoming
ready and being restored into therefore produced `FAILED` — the product's one critical alert, fired
on a container that lost a race. The conformance case reproduces it, and the fix is in two parts:
the plugin confirms the target answers before it starts the tool, and a lost connection in the
tool's output is classified as `ERROR_CODE_CONNECTION_FAILED` rather than a tool failure. This is
the first thing a new engine should be checked for.

**The suite needs an engine-specific fixture, and that is a designed extension point rather than a
leak.** Fleetward never writes to a monitored instance, so there is no RPC that can create a table
— and a backup of an empty database proves nothing, because comparing zero objects to zero objects
succeeds trivially. `test/conformance/fixtures_test.go` defines a two-method `Fixture` interface;
adding an engine means registering one beside the plugin, never changing an assertion.

The corruption cases turned out to need no manifest tampering at all, which was the point. A
truncated object and a flipped byte are altered in the bucket; the mismatched-counts case compares
one real manifest against a second real artifact taken after rows were deleted. Every number in
every assertion came out of the plugin.
