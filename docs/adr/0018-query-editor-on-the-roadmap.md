# ADR-0018: The query editor moves from non-goal to final phase

- **Status:** Accepted
- **Date:** 2026-07-27
- **Supersedes:** the "a SQL query client or GUI" entry in the original non-goals

## Context

The original brief listed a SQL query client as the first non-goal. The intent was scope discipline:
query editors are a large, well-served category, and building one early would consume the effort
that the differentiating features need.

The product owner has since asked for one — a universal tool covering the whole DBA workflow,
comparable to DataGrip. Since Fleetward already holds connections and credentials for every server
in the estate, the marginal cost of *not* having it is a context switch to another tool for the one
task a DBA performs most.

That is a reasonable argument, and the original non-goal was written without it.

## Decision

The query editor is admitted to the roadmap as **Phase G, the final phase**, and is removed from
the non-goals.

It does not begin until all of the following are true:

1. **RBAC is enforced server-side on every route**, with role and scope. A query editor inherits
   whatever authorization the platform has; if that is nothing, it grants everyone full access to
   every production database in the estate.
2. **Every execution produces an audit record** — principal, instance, statement, timestamp,
   outcome — in the append-only audit log.
3. **Read and write are distinguished before execution**, and write against a production-flagged
   environment requires typed confirmation.
4. **Statement timeouts and result limits are enforced server-side**, so a careless query cannot
   take down the instance it is run against.
5. The three compliance pillars — backup, access, structural drift — are delivered and in use.

## Consequences

- The scope discipline that made the original non-goal correct is preserved as *sequencing* rather
  than as prohibition. The differentiating work still lands first.
- The conditions above are not bureaucracy. Fleetward holds credentials for roughly fifty
  production servers. Adding arbitrary SQL execution turns it from a system that *reads* estate
  metadata into one that can *destroy* any database in the estate. That is a categorically
  different security posture, and the controls have to exist before the capability does, not after.
- Recording the conditions now means a future session cannot reasonably start Phase G early: the
  gate is written down and checkable.
- Cost: the roadmap grows a large phase. That is honest — it was always going to be wanted, and
  leaving it as a non-goal would have meant it arrived unplanned and unguarded.

## Alternatives considered

- **Keep it as a non-goal.** Rejected: the product owner wants it, the argument for it is sound, and
  a non-goal maintained against the owner's intent just gets violated later without the conditions.
- **Build it early, alongside the compliance pillars.** Rejected on both focus and safety. It would
  consume the effort the differentiating features need, and it would arrive before the
  authorization and audit machinery that makes it safe.
- **Ship a read-only query view instead.** Attractive middle ground, and it may well be how Phase G
  begins. It is a design choice within the phase rather than a reason to reorder it.
