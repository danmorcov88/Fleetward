# ADR-0032: Retention never deletes an instance's last good backup, and verification decides the floor rather than eligibility

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B5 — retention and expiry
- **Relates to:** [ADR-0022](0022-failed-and-inconclusive-are-different-answers.md),
  [ADR-0024](0024-production-readiness-is-a-slice-property.md),
  [ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
  [ADR-0031](0031-an-expiry-is-stamped-when-a-backup-is-taken.md)

## Context

A correct, purely time-based implementation of "delete anything older than thirty days" will, on a
server whose backups have been failing for five weeks, delete the last backup that worked.

That is not a bug in the implementation. It is the feature doing exactly what it was asked, on the
instance that needed it most — and it is the single most damaging thing a product whose entire
purpose is backup trustworthiness could do.

A second question arrives with it. Fleetward knows more about a backup than its age: it knows
whether a restore of it was proven to work, proven to fail, or never attempted
([ADR-0022](0022-failed-and-inconclusive-are-different-answers.md)). Three defensible positions
exist about whether that verdict should decide what gets deleted, and they lead to different
products:

- **an unverified backup is the first to go** — it proves the least, so it is the cheapest to lose;
- **an unverified backup is the last to go** — nobody has shown it is bad;
- **verification does not decide eligibility at all.**

## Decision

### Time decides what is eligible. Verification decides what the floor keeps.

**Eligibility is age alone.** A managed backup whose stamped `expires_at` has passed is a candidate,
and nothing about its verification status changes that.

**The floor keeps two rows per instance, whatever their expiry says:**

1. the most recent `succeeded` managed backup — widened to N by
   `FLEETWARD_RETENTION_MIN_KEEP`, whose default is 1;
2. the most recent managed backup whose latest verification is `VERIFIED`.

Often the same row. On a healthy instance the floor costs one artifact.

**`FLEETWARD_RETENTION_MIN_KEEP` cannot be zero.** The control plane refuses to start, the way it
already refuses a lease heartbeat longer than its TTL, and the sweep refuses again at the point of
action so that a policy assembled in code cannot bypass it either. A floor that can be configured
away is not a floor; it is a default.

### Why rule 2 is not decoration

Rule 1 alone is what most tools ship, and it fails precisely on the estate this product exists for.
Take a server whose backups have been succeeding and failing verification for a month. Rule 1 keeps
the newest backup — which is *known to be unrestorable* — and deletes the last one that was proven
good. The floor would then be holding the one artifact demonstrated to be worthless.

Rule 2 keeps the proof. It is the difference between "we kept something" and "we kept something that
works".

### Why a `FAILED` verification does not accelerate deletion

It is tempting: the artifact is known bad, so why keep it. It is wrong, because a failed
verification is the loudest signal this product has, and **the artifact is the evidence behind it**.
Deleting it early destroys the investigation an operator is about to start. It expires on its
ordinary schedule like everything else.

### Why eligibility stays a subtraction a human can do

An operator has to be able to look at `fleetward-cli backup retention` and know it is right. *Taken
more than N days ago* is checkable in their head. The moment eligibility depends on a verdict, "why
did that one go and this one stay" becomes a question only the source code answers — and this is the
one feature where an operator not being able to predict the behaviour is itself the failure.

## Consequences

**Between one and two artifacts per instance become undeletable through Fleetward.** On an estate of
fifty, up to a hundred artifacts pinned indefinitely. Rule 2 can pin an old artifact forever on an
instance that never verifies successfully again, and that artifact is by definition the oldest thing
worth keeping there.

**There is currently no way to reclaim that storage short of deleting the instance.** There is no
`DeleteBackup` action — a human deleting one named backup is a different feature with a different
safety story (confirmation, audit, RBAC) and it belongs after B6 — and
`DeleteInstance(delete_artifacts=true)` is declared in the contract and still unimplemented. This is
stated plainly rather than left to be discovered, and it is the price of the floor.

**The preview must explain the floor, not just apply it.** `PreviewRetention` returns every backup
that is past its expiry and kept anyway, each with a sentence saying which rule holds it. Without
that, the floor is indistinguishable from a bug, and an operator would go looking for one.

**The floor is computed per instance over every owned backup, not over the expired ones.** If an
instance's three newest backups are all still young, the floor is already satisfied by them and the
old ones are free to go. Computing it over candidates alone would keep one extra artifact per
instance forever — a floor with an off-by-one is a slow leak.

## Alternatives considered

**No floor: purely time-based retention.** The literal reading of the request, and the most
predictable behaviour there is. Rejected because its ordinary, correct operation deletes the last
working backup of the sickest server in the estate. Predictability is worth a great deal and it is
not worth that.

**Keep only the newest backup, with no regard to verification.** One rule, easier to explain, and it
prevents the empty-instance case. Rejected because of the instance that has been failing
verification for a month: it keeps a backup known to be bad and deletes the one known to be good,
which is the worse of the two possible mistakes and is not obviously so until it happens.

**Delete unverified backups first.** Superficially attractive — an unverified artifact proves the
least. Rejected because "unverified" almost always means *nobody checked*, not *it is bad*: a Docker
daemon that went away, a `sampled` policy at 5%, a verification queue that has been backed up for a
week. An estate where verification is failing to run would then lose its backups faster precisely
because it is less healthy, which is the wrong direction for every incentive in the system.

**Delete `FAILED` backups first.** Rejected: it destroys the evidence behind the loudest alert the
product raises, at the moment somebody is most likely to want to look at it.

**Make the floor per schedule rather than global.** More expressive, and it needs a migration and a
new column on `schedules`. Rejected for this slice as speculative: nobody has yet asked for different
floors on different instances, and the smaller surface is the deliberate choice. A global knob with a
refused zero is the whole of it, and widening it later is additive.
