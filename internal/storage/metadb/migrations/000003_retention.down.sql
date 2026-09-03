-- Reverses 000003_retention.up.sql.
--
-- Simpler than 000002's reversal, and for a reason worth stating: nothing here widened a CHECK, so
-- nothing here has to delete rows to narrow one back. `expired` was already a legal backup state in
-- migration 000001; this migration only forbade it for observed rows, and dropping that constraint
-- forbids nothing.
--
-- Rows that reached `expired` are left exactly as they are. Their artifacts are gone from object
-- storage and no down migration can bring them back, so rewriting them to `succeeded` would
-- manufacture a row claiming a restorable artifact that does not exist — which is the one state
-- this whole slice exists to prevent.

DROP INDEX IF EXISTS idx_backups_artifact_pending_delete;

ALTER TABLE backups DROP CONSTRAINT IF EXISTS backups_observed_never_expires;

ALTER TABLE backups DROP COLUMN IF EXISTS artifact_deleted_at;
