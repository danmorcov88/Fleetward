# Engineering journal

One entry per delivered slice: what shipped, what was verified and how, the decisions worth
carrying forward, and what was deliberately left unbuilt.

These entries are **append-only**. An entry records what was true and why it was decided at the time
it was written, and is never edited afterwards — a decision that is later reversed is recorded as a
reversal in the newer entry, not by quietly rewriting the older one. That is what makes the journal
evidence rather than a summary.

One consequence: the Phase A entries refer to "Phase C", "Phase E", and "Phase F", the roadmap
vocabulary in use when they were written. [`../../roadmap.md`](../../roadmap.md) is the current plan,
and Phase F no longer exists at all
([ADR-0024](../../adr/0024-production-readiness-is-a-slice-property.md)). Read those references as
"a later slice".

The journal exists because this content used to live in `STATUS.md`, which grew to 586 lines mixing
three documents with three different lifetimes: current status (rewritten every slice), historical
rationale (never rewritten), and the roadmap (duplicated from three other files). Splitting them by
lifetime is what keeps each one trustworthy. Status now answers *where are we*; the journal answers
*why is it like this*; [`../../roadmap.md`](../../roadmap.md) answers *where are we going*.

The reader in a hurry should start with [`../design-notes.md`](../design-notes.md), which selects
the decisions with the longest reach and links back to the entries they came from.

| Entry | Delivered | Slice brief |
|---|---|---|
| [Foundation](00-foundation.md) — contract, control plane, dev stack | 2026-07-27 | — |
| [A1](A1-health-and-discover.md) — PostgreSQL health checks and discovery | 2026-07-27 | — |
| [A2](A2-inventory-and-cli.md) — inventory, credential storage, instance CLI | 2026-07-30 | [brief](../slices/A2-inventory-and-cli.md) |
| [A3](A3-sandbox-provider.md) — Docker sandbox provider, teardown guaranteed | 2026-08-29 | [brief](../slices/A3-sandbox-provider.md) |
| [A4](A4-backup-and-manifest.md) — backups with a source manifest | 2026-08-29 | [brief](../slices/A4-backup-and-manifest.md) |
| [A5](A5-restore-and-verify.md) — restore into a sandbox, and prove it | 2026-08-31 | [brief](../slices/A5-restore-and-verify.md) |
| [A6](A6-verification-fails-loudly.md) — verification fails loudly | 2026-08-31 | [brief](../slices/A6-verification-fails-loudly.md) |
| [B1](B1-scheduler-and-leases.md) — the scheduler and the job lease | 2026-09-02 | [brief](../slices/B1-scheduler-and-leases.md) |
| [B2](B2-sqlserver-plugin.md) — SQL Server, and the claim it was written to test | 2026-09-02 | [brief](../slices/B2-sqlserver-plugin.md) |
| [B3](B3-observed-backups.md) — reporting on backups Fleetward did not take | 2026-09-02 | [brief](../slices/B3-observed-backups.md) |
| [B4](B4-estate-overview.md) — fifty servers at a glance, and a contract the client can trust | 2026-09-03 | [brief](../slices/B4-estate-overview.md) |
| [B5](B5-retention-and-expiry.md) — the first slice that can destroy data | 2026-09-03 | [brief](../slices/B5-retention-and-expiry.md) |

## Writing an entry

Closing a slice means adding one file here, named `<slice>-<short-slug>.md`, and rewriting
[`../STATUS.md`](../STATUS.md) to point at the next slice. The shape that the Phase A entries
settled into, and that later ones should keep:

1. **What shipped** — a paragraph naming the packages, not a changelog.
2. **How it was verified** — the actual command, the actual numbers, the platform, the date. *"A
   backup of the stack's own PostgreSQL 16 produced a 66.9 KiB artifact"* is evidence; *"backups
   work"* is not.
3. **Decisions worth carrying forward** — the reasoning, especially where the obvious choice was
   rejected. This is the part that is worth writing.
4. **Not built, deliberately** — and which slice owns it. A gap that is named is a scope fence; a
   gap that is silent is a bug waiting to be rediscovered.
5. **Still open** — anything known-broken, so `STATUS.md` can list it without re-deriving it.
