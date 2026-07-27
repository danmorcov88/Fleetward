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
