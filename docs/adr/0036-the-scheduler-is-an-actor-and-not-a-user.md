# ADR-0036: The scheduler is an actor string and not a user, and the tenant comes from the caller

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B6 — the authorization spine
- **Relates to:** [ADR-0008](0008-oidc-rbac-multitenancy.md),
  [ADR-0013](0013-internal-scheduler-with-leases.md),
  [ADR-0030](0030-retention-sweeps-the-estate-and-never-deletes-a-row.md),
  [ADR-0033](0033-the-bootstrap-credential-is-configuration-and-never-a-row.md)

## Context

Two thirds of what Fleetward does has no human behind it. The scheduler materializes jobs and runs
backups; the retention sweep deletes artifacts; the reaper closes jobs whose runner stopped
reporting. `jobs.triggered_by`, `backups.triggered_by` and `verifications.triggered_by` have all been
nullable since migration 000001 precisely for that case.

But §7.5 of `CLAUDE.md` says *every* mutating action lands in `audit_log`, and a backup is mutating.
So "who did this" needs an answer for the majority of rows, and "nobody" is not one — the question
an operator actually asks about a missing artifact is *what* deleted it, and the answer is
retention.

A second question arrives with the same slice. Four services held `metadb.DefaultTenantID` as a
field and dereferenced it at 72 sites. [ADR-0008](0008-oidc-rbac-multitenancy.md) put `tenant_id` on
every table from the first migration specifically so that tenancy would never be a later migration —
and in three years of schema that claim had never once been exercised by code.

## Decision

**A system caller is an actor string, not a user row, and it is not exempt from auditing.**
`audit_log.user_id` is NULL and `actor` reads `system:scheduler`, `system:retention`,
`system:backup`. The `triggered_by` columns stay NULL for that work and carry the user id for
anything a human asked for, which is what nullable was always for.

**The tenant comes from the principal on the request's context.** The four `tenantID` fields are
gone. `authn.Tenant(ctx)` is what every query reads, and the scheduler, the retention sweep and the
background half of a running backup each attach a system principal carrying the tenant they are
working in.

## Consequences

**A system principal has no credential, and that is what makes it safe.** It is constructed
in-process and nothing that parses an HTTP request can produce one, so it can never be presented at
the port. Giving the scheduler a `users` row would have created an identity that could be granted
roles and — the moment anything issued it a token — impersonated. The audit log gets the attribution
without the identity.

**"Who deleted this artifact" answers `system:retention`.** Not `system:scheduler`, though the sweep
runs on the scheduler's tick, because those are different facts and a log that spelled them the same
way could not tell them apart afterwards.

**A code path that reaches the database with no principal fails loudly.** `authn.Tenant` returns the
empty string, every query filters `tenant_id = $1` against a UUID column, and Postgres rejects the
statement. That is the intended failure and it is the whole reason the refactor was worth its risk:
the alternative failure — a hardcoded constant quietly serving the default tenant to a caller who
belongs to another — is silent, and would have been discovered by a customer.

**The background half of a backup takes its lifetime from the service and its tenant from the
caller.** A backup outlives the request that asked for it, so the work continues on a context the
service owns. Inheriting a hardcoded tenant there would have written one tenant's backup rows under
another's, which is the same bug the refactor removed, reintroduced in the one place nobody would
look.

**The human is recorded once, not twice.** `backups.triggered_by` and the audit row written by the
API layer both name whoever pressed the button; the hours of work that follow are attributed to
Fleetward. Attributing a failure at 04:00 to whoever happened to click at 21:00 would be a worse
record rather than a more complete one.

**`metadb.DefaultTenantID` survives in exactly two places**: the seed, and the system principals
that name the tenant they operate in. A second tenant is not created by anything, and creating one
is a later slice — but nothing in the query layer now assumes there is only one.

**The cost was 72 mechanical call sites in code that deletes data.** The queries themselves are
unchanged, byte for byte, including B5's retention SQL; only where the value comes from moved. That
is what made it a compiler-checked substitution rather than a rewrite, and it is why it was done in
this slice rather than deferred to one where it would have been no cheaper.

## Alternatives considered

**A `users` row for the scheduler.** The tidy-looking option: `triggered_by` is never NULL, joins
are simpler, and every audit row has a user. Rejected because it manufactures an identity whose only
purpose is to be referenced, and identities that exist can be granted things and can acquire
credentials.

**Exempting automatic work from the audit log.** Cheapest, and it deletes two thirds of the log's
value. The rows an operator most wants after an incident are the ones nobody was watching.

**Leaving the tenant a constant and plumbing it in a later slice.** Smaller, and it leaves
ADR-0008's central claim untested through B10, after which an identity provider would sit on top of
a query layer that still could not distinguish tenants. There is no later slice for which this is
cheaper.

**Deriving the tenant in the middleware and passing it as an argument** rather than on the context.
Explicit, and it would have changed 72 function signatures instead of 72 expressions, through the
same code. The context already carries the request id and the principal; adding a second mechanism
for a value that travels with them would have been the inconsistent choice.
