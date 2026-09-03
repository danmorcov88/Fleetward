# ADR-0034: Grants are additive and the highest rank wins, because the schema has no way to say "deny"

- **Status:** Accepted
- **Date:** 2026-09-03
- **Slice:** B6 — the authorization spine
- **Relates to:** [ADR-0008](0008-oidc-rbac-multitenancy.md),
  [ADR-0035](0035-enforcement-is-a-policy-table-and-a-decorator.md)

## Context

`role_grants` has existed since migration 000001 and has never held a row. It binds a user to a role
within a scope, and it carries a constraint:

```sql
CONSTRAINT role_grants_single_scope
    CHECK (environment_id IS NULL OR instance_id IS NULL)
```

So a grant covers one instance, or one environment, or — both NULL — the whole tenant. Never two of
those at once.

That makes an obvious question unavoidable the moment anything reads the table. A user holds
`viewer` on environment *staging* and `dba` on instance *staging-3* inside it. And, the pairing that
actually matters, a user holds `dba` on environment *staging* and `viewer` on instance *staging-3*.

Two rules are available and they disagree about the second case:

- **Most specific wins.** The instance grant overrides the environment grant, so the second user is
  a `viewer` on *staging-3* and a `dba` everywhere else in staging.
- **Highest rank wins.** Grants add up, so the second user is a `dba` on *staging-3* as well.

"Most specific wins" is the rule most people expect, because it is how file permissions and most
policy languages behave.

## Decision

**The effective role for a scope is the maximum rank of every grant that covers it. Grants are
additive: a grant only ever adds permission, and never removes any.**

So a `dba` grant on one instance elevates its holder within a `viewer` environment, and a `viewer`
grant on one instance does **not** demote a `dba` environment grant.

The ranks come from the `roles` table — viewer 10, operator 20, dba 30, admin 40, seeded by
migration 000001 — read at startup rather than declared as Go constants, because a constant that
disagreed with the table would be a bug nothing surfaced until somebody edited one of the two.

## Consequences

**There is no way to express "this person, but not on that one server".** That is the real cost, it
is the case an operator will eventually want, and the answer is that they must not grant the wider
role in the first place — grant instance by instance instead.

**What is gained is that authorization cannot silently weaken.** Under "most specific wins", adding
a narrow `viewer` grant to somebody who already holds a wide `dba` grant *reduces* what they can do,
which is an outcome nobody typing `token create --role viewer` is expecting. Worse, it makes
`role_grants` behave like a deny list — and the schema has no deny column, no ordering between
grants of equal specificity, and no way to say "deny" explicitly. A deny mechanism that exists only
as an emergent property of a resolution rule is a security control that looks like it works, works
by accident, and stops working the day somebody adds a second grant at the same level.

**The rule is one sentence, which matters more here than elsewhere.** An operator has to be able to
predict what a grant does before running the command. "Everything you have been given, added up" is
predictable; "the most specific one, where specificity is instance > environment > tenant, and
equal-specificity grants resolve by..." is not.

**Adding a deny is a schema change and a new ADR**, not a change to the resolver. That is written
down here so a future session that reaches for "most specific wins" as an obvious improvement finds
out first that it was considered and what it costs.

**A scope-less request needs a tenant-wide grant.** The same rule read from the other end: since
scope comes from the request ([ADR-0035](0035-enforcement-is-a-policy-table-and-a-decorator.md)), a
request that names no instance and no environment is asking about the whole estate, and only a grant
covering the whole tenant covers that. An instance-scoped `dba` can list *their* instance's backups
and cannot list the estate's. That is restrictive rather than permissive, which is the safe
direction for a rule to be wrong in, and it is what stops a list endpoint returning a row from
outside the caller's scope without any per-endpoint filtering.

## Alternatives considered

**Most specific wins.** Rejected above. The deciding argument is not that it is less useful but that
what it produces — a deny mechanism nobody declared — is worse than the limitation it removes.

**A deny flag on `role_grants`.** The honest way to get the missing behaviour: an explicit column, an
explicit precedence rule, and a UI that shows it. Rejected for this slice on scope, and it would
supersede this record rather than amend it.

**Resolving only the single most relevant grant and ignoring the rest.** A simplification of "most
specific wins" with the same flaw and less predictability.

**Refusing to store overlapping grants at all**, so the question cannot arise. Attractive, and it
breaks the ordinary case this design is for: a tenant-wide `viewer` plus a `dba` on the three
servers somebody actually operates is exactly the shape an estate of fifty wants.
