-- Reverses 000001_init.up.sql.
--
-- Order matters: dependants before their referents. The audit_log trigger is dropped explicitly
-- because it would otherwise block the DROP TABLE it guards.

DROP TRIGGER IF EXISTS audit_log_no_update ON audit_log;

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS notifiers;
DROP TABLE IF EXISTS alerts;
DROP TABLE IF EXISTS alert_rules;
DROP TABLE IF EXISTS restores;
DROP TABLE IF EXISTS verifications;
DROP TABLE IF EXISTS backups;
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS schedules;
DROP TABLE IF EXISTS role_grants;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS connections;
DROP TABLE IF EXISTS secrets;
DROP TABLE IF EXISTS instances;
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;

DROP FUNCTION IF EXISTS set_updated_at();
DROP FUNCTION IF EXISTS audit_log_is_append_only();
