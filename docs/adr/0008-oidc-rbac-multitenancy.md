# ADR-0008: OIDC authentication, scoped RBAC, and day-one multi-tenancy

- **Status:** Accepted
- **Date:** 2026-07-26

## Context

Fleetward can trigger restores. A restore is a destructive, irreversible operation against
production data. Authorization is therefore a correctness requirement, not a feature.

Organizations already have an identity provider; asking them to manage a second user directory is a
non-starter. And retrofitting tenancy onto a schema is one of the most expensive migrations a
project can undertake.

## Decision

- **AuthN:** OIDC. Dex in the dev compose stack; any compliant IdP in production.
- **AuthZ:** RBAC with four ordered roles — `viewer < operator < dba < admin` — and a scope of
  environment → instance. A grant is (role, scope) for a principal.
- **Enforcement is server-side, on every route, without exception.** The UI hides actions a user
  cannot perform, but hiding is a courtesy, never a control.
- **Multi-tenancy:** `tenant_id` on every metadata table from the very first migration, even while
  the MVP runs single-tenant.

## Consequences

- No local password storage, no password reset flow, no credential-handling liability.
- The `viewer` cannot trigger backup/restore is a testable acceptance criterion (§7.5) rather than
  an aspiration.
- Carrying `tenant_id` costs a column and a WHERE clause now; adding it later would cost a
  full-schema migration plus an audit of every query.
- Cost: a dev-time IdP dependency. Dex in compose keeps the quickstart to one command.

## Alternatives considered

- **Local user accounts.** Faster to build, but duplicates the customer's identity source and makes
  us responsible for credential storage.
- **Flat global roles.** Insufficient: a DBA responsible for staging must not be able to restore
  production.
- **Deferring tenancy to Phase 2.** Rejected — see the migration cost above.
