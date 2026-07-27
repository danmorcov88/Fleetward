# ADR-0015: Observed and managed backups

- **Status:** Accepted
- **Date:** 2026-07-27

## Context

Fleetward's original design assumed it takes the backups. That assumption is fine for a greenfield
estate and wrong for every real one.

The user this product exists for administers roughly fifty servers whose backups are already being
taken — by cron, by scripts, by native scheduling, by tooling that predates Fleetward. Their most
acute problem is not *taking* backups. It is that they cannot physically verify, within a working
week, whether all fifty ran on schedule and succeeded.

A tool that requires migrating fifty production servers' backup arrangements before it shows
anything useful will not be adopted. Migrating production backup arrangements is exactly the kind of
change one makes *after* trusting a tool, not in order to start trusting it.

## Decision

Fleetward recognizes two origins for a backup, and the distinction runs through the contract, the
schema, and the UI.

- **`observed`** — evidence of a backup Fleetward did not take. The plugin reports what it can see:
  timestamps, sizes, locations, success or failure where the engine records it. Fleetward evaluates
  schedule adherence and surfaces failures, but does not own the artifact.
- **`managed`** — a backup Fleetward ran. Only these carry a `SourceManifest` Fleetward captured at
  backup time, a checksum it computed, and an artifact it controls.

Consequences of the split that must not be blurred:

- **Only managed backups can be fully verified.** Verification compares a restored instance against
  a manifest captured at backup time. An observed backup has no such manifest, so at most its
  restorability can be smoke-tested — its *contents* cannot be attested to. The UI must never
  present these two as the same green checkmark.
- Compliance evaluation — did a run happen inside its window, did it succeed — applies equally to
  both. That is the whole point: adherence is answerable on day one, for the entire estate.

The contract gains an additive RPC, `ListBackupHistory`, and a capability declaring whether a plugin
can see backup evidence at all. Engines differ enormously here: PostgreSQL exposes
`pg_stat_archiver` and `backup_label`, while another engine may offer only files on disk in a
configured directory. What a plugin can observe is therefore a capability, not an assumption.

## Consequences

- Fleetward becomes adoptable on an existing estate without changing anything about it, which is
  the difference between a tool that gets evaluated and one that gets installed.
- Users can migrate to managed backups per instance, at their own pace, once they trust the tool.
- Cost: two backup origins is real domain complexity. Every query, screen, and alert has to be clear
  about which it is describing. The risk we are accepting is that a user mistakes an observed backup
  for a verified one — which is precisely the false confidence this product exists to eliminate, so
  the two-part status display is not a UI nicety here, it is a correctness requirement.
- `ListBackupHistory` is additive, so `buf breaking` stays green.

## Alternatives considered

- **Managed only, as originally planned.** Conceptually clean, and it makes verification uniformly
  meaningful. Rejected because it inverts the adoption sequence: it demands the riskiest possible
  change to production before delivering any value.
- **Observed only.** Simpler, and it would answer the adherence question. Rejected because
  verification — the product's actual differentiator — is impossible without a manifest captured at
  backup time, which requires having taken the backup.
- **A generic "import backup metadata" API instead of a plugin RPC.** Would push the engine-specific
  work onto the user's own scripts, exactly the knowledge the plugin contract exists to encapsulate.
