# ADR-0017: Access compliance is read-only, with generated remediation

- **Status:** Accepted
- **Date:** 2026-07-27

## Context

A DBA managing an estate needs to answer, for every server: who has access, with what role, when
the account was created, whether it has an expiry, and whether that expiry has passed. The purpose
is concrete — identify non-compliant accounts so they can be removed.

That last word invites Fleetward to *do* the removing. The original contract forbids it:
`ListPrincipals` is documented as strictly read-only, on the grounds that Fleetward surfaces access
rather than administering it.

The question is whether that rule should survive contact with the actual need.

## Decision

It survives, for now, with an addition.

Fleetward **detects and reports**, and **generates the remediation SQL**, but does not execute it.

The compliance engine evaluates policies against what `ListPrincipals` returns:

- accounts past their expiry
- accounts with no expiry at all, where policy requires one
- superusers not on an expected list
- accounts dormant beyond a threshold
- privileges granted directly rather than through a role, where policy requires roles

For each finding it produces the exact statement that would fix it — `ALTER ROLE … VALID UNTIL …`,
`REVOKE …` — presented for a human to review and run.

`Principal` gains a `created_at` field, the one thing the contract is missing for this. Everything
else needed is already there: `password_expires_at`, `last_login_at`, `is_superuser`, `can_login`,
and `privileges`.

## Consequences

- The user gets the full answer to "who is non-compliant, and what is the fix" without Fleetward
  ever holding write access to database access control.
- **A bug in Fleetward cannot lock anyone out of production.** This is the entire reason for the
  decision. A false positive in a read-only report costs someone two minutes of review. The same
  false positive with execution attached revokes a service account at 3am. Those failure modes are
  not comparable, and the asymmetry justifies the friction.
- Generating the statement rather than only describing the problem means the friction is small: the
  operator reviews and pastes, rather than reconstructing the fix themselves.
- Fleetward's own database credentials can stay read-only on monitored instances, which materially
  shrinks what a compromise of Fleetward yields.
- Cost: remediation is manual, and at fifty servers that is real work. Revisit when RBAC
  enforcement, the audit log, and typed confirmation are all in place — the conditions under which
  a destructive action is defensible. That revisit is a new ADR superseding this one, not a quiet
  change.

## Alternatives considered

- **Execute remediation directly.** What the user initially asked for, and genuinely more useful.
  Rejected for now on blast radius: it requires Fleetward to hold privileged credentials on every
  monitored instance, and it makes a reporting bug into an outage.
- **Report only, without generating SQL.** Safer still, and less useful. The engine-specific
  knowledge of *how* to fix a finding is exactly what the plugin contract exists to hold; making
  the operator supply it wastes the plugin.
- **Approval workflow with Fleetward executing after sign-off.** The right eventual answer. It
  needs RBAC enforcement and the audit log first, neither of which exists yet.
