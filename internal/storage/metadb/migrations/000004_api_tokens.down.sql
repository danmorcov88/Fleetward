-- Reverses 000004_api_tokens.up.sql.
--
-- Dropping api_tokens destroys every credential ever issued, which is the intended reading of a
-- down migration here: after this runs, nothing can authenticate except the bootstrap token, which
-- is configuration and was never in the database to begin with.
--
-- `users`, `role_grants` and `audit_log` are left exactly as they are. They predate this migration
-- and did not come with it, and audit_log could not be cleaned even on purpose — DELETE is refused
-- by audit_log_no_update, which is the property that makes it evidence.

DROP INDEX IF EXISTS idx_audit_time;

DROP TABLE IF EXISTS api_tokens;
