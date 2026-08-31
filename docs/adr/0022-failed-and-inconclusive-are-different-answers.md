# ADR-0022 — A verification distinguishes a bad backup from broken machinery

- **Status:** Accepted
- **Date:** 2026-08-31
- **Slice:** A5 — restore into a sandbox and verify
- **Relates to:** [ADR-0012](0012-testcontainers-and-conformance-suite.md),
  [ADR-0020](0020-sandbox-credentials-from-template-placeholders.md),
  [ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md)

## Context

`VerificationStatus` has carried three values since the contract was first written: `VERIFIED`,
`FAILED`, and `INCONCLUSIVE`. Implementing verification turned that from a tidy enum into the
decision the whole feature rests on, because almost everything that can go wrong during a
verification is *not* evidence about the backup.

A verification pulls a container image, starts a database, downloads an artifact over a network the
plugin does not control, shells out to a native tool, and only then compares row counts. Of those
five steps, exactly one is a statement about whether the backup is any good. If the other four
report `FAILED`, then the alert that means "a backup you believe in cannot be restored" fires
routinely — on a slow registry, on a full disk, on a host that is missing `pg_restore`. An alert
that fires routinely is muted, and the product's one differentiating signal is gone.

The failure mode is asymmetric and both directions are bad:

- Reporting infrastructure trouble as `FAILED` destroys trust in the alert.
- Reporting a bad artifact as `INCONCLUSIVE` hides data loss, which is the thing this product
  exists to find.

So the classification cannot be a judgement call made at each call site. It has to be a rule, and
the rule has to be legible from the wire.

Two specific cases forced the design.

**A checksum mismatch and a broken download are discovered in the same place.** The plugin fetches
the artifact through a presigned `GET` and hashes it on the way past. A connection reset and a
mismatched hash both surface while reading the same response body, and the obvious error code for
both is `ERROR_CODE_OBJECT_STORE_FAILED`. One is a flaky network; the other is the loudest thing
Fleetward can report. Indistinguishable by code, they would have to be told apart by string
matching on the message — precisely the coupling a typed plugin contract exists to prevent.

**`pg_restore` exits non-zero on healthy restores.** A dump refers to roles that exist on the source
cluster and nowhere else; a comment on an extension can only be set by a superuser; the template
database already provides `public`. Restoring into an empty sandbox produces one or more of these
almost every time. Treating any non-zero exit as a failed verification would report failure on
every healthy restore of a real database.

## Decision

**`FAILED` is reserved for evidence about the artifact. Everything else is `INCONCLUSIVE`.**

Three mechanisms implement it.

### 1. A plugin says when it is blaming the artifact

The SDK gains a detail key and a constructor:

```go
sdk.ArtifactCorrupt("the artifact does not match its checksum: …")
// → PluginError{Code: INVALID_ARGUMENT, Details: {"artifact": "corrupt"}}

sdk.IsArtifactCorrupt(pe) // core's side of the same convention
```

`PluginError.details` is a `map<string, string>` that already exists in the contract, so this needs
no proto change and no new error code. Core reads it, never a message. A plugin that does not set it
is simply never given the benefit of `FAILED` on that path, which is the safe direction: the worst
outcome is an inconclusive verification an operator has to look at, rather than a silent pass.

Core's rule is then exactly two lines wide:

- the plugin blamed the artifact, or the engine's own tooling refused to load it
  (`ERROR_CODE_TOOL_FAILED`) → `FAILED`
- anything else → `INCONCLUSIVE`

### 2. The checksum is confirmed before a single statement is applied

The artifact is written to a private temporary file and hashed in full before the restore tool is
started, rather than piped into it. This costs local disk equal to the compressed artifact — the
cost ADR-0021 deliberately refused to pay on the *backup* path — and it is worth paying here for two
reasons the backup path does not share. A verification already occupies a whole container, so it is
not the lightweight operation a backup is; and restoring a corrupted artifact and only then noticing
the counts are wrong wastes minutes and reports the wrong cause. "The bytes are not the bytes we
wrote" and "the data does not match" are different diagnoses, and only the first one is true.

### 3. The restore step is lenient; the count comparison is strict

`pg_restore` runs with `--no-owner --no-privileges --no-comments`, which removes most of the
cosmetic failures at the source. What remains is classified against a short, specific list of
patterns — a missing role, an object that already exists, an ownership the sandbox user does not
have. Those are recorded on the result and waved through. Anything else is fatal.

This is safe only because of the ordering: the restore step is allowed to be lenient precisely
because something stricter runs immediately after it. A restore that silently dropped a table cannot
survive a per-table row count comparison against the manifest, so the honest arbiter is the
comparison, not the exit code. The waived diagnostics travel in `RestoreResult.metadata` so an
operator can see what was waved through rather than having to trust that it was reasonable.

### And a fourth thing that is not a status at all

**A backup with no manifest is `INCONCLUSIVE` by construction, and never reaches a sandbox.**
Comparing zero objects to zero objects succeeds trivially, so the naive implementation of this
feature reports `VERIFIED` for a backup that proves nothing — the single most dangerous answer the
system can give. Core refuses before provisioning, and the plugin refuses again if it is ever handed
one, because the two halves are separately implementable and this is not a check to have in only one
of them.

## Consequences

**An operator has to understand three states rather than two.** That is a real cost in the UI, and
Phase B's Estate Overview has to render it: `INCONCLUSIVE` is not "amber for failed", it is "we did
not check". The compensating benefit is that `FAILED` can be wired to a critical alert with no
qualification at all.

**The `artifact: corrupt` detail is a convention, not a type.** It lives in the SDK so both sides
share one definition, and the conformance suite is where a plugin that gets it wrong will be caught.
A dedicated `ErrorCode` would be stronger; it was not added because enum values are cheap to add
later and expensive to add speculatively, and because the detail carries the same information with no
contract change.

**The cosmetic-diagnostic list is engine-specific and lives in the plugin, where it belongs.** Core
never sees it. Another engine's plugin will need its own, and the fact that the list is short and
specific rather than a broad pattern is the thing to preserve: a pattern that matched too widely
would waive a real failure, and a verification that waives real failures is worse than no
verification, because it reports a confidence nobody earned.

**Spooling the artifact to disk sets a floor on the host running the plugin.** It is bounded by the
artifact size and released as soon as the restore ends, but a fifty-server estate verifying on a
schedule will want that bounded further. `SandboxConfig` has no knob for concurrent verifications
today; that is the same gap slice A3 recorded, and it now has a second reason to be closed.
