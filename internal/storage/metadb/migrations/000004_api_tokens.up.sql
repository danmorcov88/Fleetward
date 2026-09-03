-- The authorization spine (ADR-0008, ADR-0033, ADR-0034, ADR-0035, ADR-0036).
--
-- Almost nothing is added here, and that is the point. `users`, `roles` (seeded, with ranks),
-- `role_grants` (scoped, constrained) and `audit_log` (append-only by trigger) have all existed
-- since migration 000001 and have never held a row. This migration adds the one thing the original
-- schema could not have: a credential, because ADR-0008 assumed an identity provider would supply
-- one and B10 is where that arrives.

-- -----------------------------------------------------------------------------------------------
-- 1. API tokens
-- -----------------------------------------------------------------------------------------------
--
-- A token is presented as `fwt_<id>_<secret>`. Only the id half is stored in the clear; the secret
-- is stored as its SHA-256 and compared in constant time.
--
-- SHA-256 rather than a password KDF, deliberately. The secret is 128 bits from crypto/rand, not a
-- passphrase — there is no dictionary to attack and no work factor that would help against one that
-- does not exist. A KDF here would buy nothing and would put tens of milliseconds on the hot path
-- of a screen that refetches every thirty seconds across fifty rows (ADR-0033).
--
-- The id half exists so that lookup is an equality on an indexed column rather than a scan
-- comparing every hash in the table.
CREATE TABLE api_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id     UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- The public half, carried in the credential itself so a lookup is a single indexed row.
    token_id    TEXT        NOT NULL UNIQUE,
    -- SHA-256 of the secret half, hex. The secret is returned once at creation and never stored.
    token_hash  TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_by  UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL means the token does not expire on its own. Revocation is the other way it stops.
    expires_at  TIMESTAMPTZ,
    -- Updated opportunistically, outside the request, so an operator can tell a live credential
    -- from one nobody has used since it was issued.
    last_used_at TIMESTAMPTZ,
    -- Set rather than deleted: audit_log rows reference the user this token authenticated, and a
    -- revoked credential that leaves no trace is a revocation nobody can prove happened.
    revoked_at  TIMESTAMPTZ
);

COMMENT ON TABLE api_tokens IS
    'Bearer credentials for humans and scripts, until OIDC arrives in B10. A token is presented as '
    'fwt_<token_id>_<secret>; only the SHA-256 of the secret is stored (ADR-0033).';

COMMENT ON COLUMN api_tokens.revoked_at IS
    'Revocation sets this rather than deleting the row, so the audit log''s references resolve and '
    'so the revocation itself is evidence.';

CREATE INDEX idx_api_tokens_user ON api_tokens (user_id);
CREATE INDEX idx_api_tokens_tenant ON api_tokens (tenant_id);

-- -----------------------------------------------------------------------------------------------
-- 2. The audit log grows, and nothing prunes it
-- -----------------------------------------------------------------------------------------------
--
-- This index is not for a screen. It exists so that the question "how much of this table is older
-- than N months" is a cheap one to ask, because the answer is the input to a decision this slice
-- deliberately does not make.
--
-- `audit_log` is append-only by trigger: UPDATE and DELETE both raise. That is the entire point of
-- the trigger, and it means retention cannot prune it the way B5's sweep prunes artifacts. The
-- mechanism that does not fight the trigger is monthly range partitioning with DROP PARTITION,
-- which is a schema change with its own failure modes and deserves its own decision rather than a
-- line in the slice that created the evidence in the first place.
--
-- The size, so nobody has to derive it: fifty instances with a nightly backup, its verification and
-- an observation poll produce roughly 150 rows a day — about 55,000 a year, tens of megabytes. It
-- is not the object store. It is also not bounded, and saying so is the honest version.
CREATE INDEX idx_audit_time ON audit_log (occurred_at);

COMMENT ON INDEX idx_audit_time IS
    'Supports asking how old the audit log''s oldest rows are. Nothing prunes this table: DELETE is '
    'refused by audit_log_no_update, and a pruning story needs partitioning and an ADR.';

-- -----------------------------------------------------------------------------------------------
-- 3. What this migration deliberately does NOT do
-- -----------------------------------------------------------------------------------------------
--
-- It seeds no user and no role grant.
--
-- A seeded administrator would be a credential that outlives the configuration that created it: it
-- would survive the operator removing every environment variable, it would be invisible in a diff,
-- and revoking it would require knowing it existed. The first credential on a fresh installation
-- comes from FLEETWARD_AUTH_BOOTSTRAP_TOKEN instead, which is configuration and never a row —
-- delete the setting and the access is gone, with nothing left behind to find later (ADR-0033).
--
-- It also invents no roles. `roles` was seeded in migration 000001 with viewer 10, operator 20,
-- dba 30 and admin 40, and `role_grants.role_name` is ON DELETE RESTRICT against it. The ranks are
-- facts in this database, read at runtime, rather than constants in Go that can quietly disagree
-- with it.
