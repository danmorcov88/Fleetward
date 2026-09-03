-- Reverses 000002_observed_backups.up.sql.
--
-- Narrowing a CHECK is not the mirror image of widening one: PostgreSQL validates the new
-- constraint against the rows that are already there, so a down migration that only swaps the
-- constraint back fails on exactly the rows the up migration made possible. Deleting them first is
-- deliberate rather than convenient — an observed backup is evidence Fleetward read from an engine
-- and can read again on the next poll, and an observe schedule is a declaration that would have
-- nothing to run it. Neither is data only Fleetward holds.
--
-- Order matters: rows first, then the constraints they violate, then the columns.

DELETE FROM backups WHERE origin = 'observed' OR state = 'unknown';
DELETE FROM jobs WHERE kind = 'observe';
DELETE FROM schedules WHERE kind = 'observe';

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check
    CHECK (kind IN ('backup', 'verify', 'restore', 'discovery', 'metrics'));

ALTER TABLE schedules DROP CONSTRAINT schedules_kind_check;
ALTER TABLE schedules ADD CONSTRAINT schedules_kind_check
    CHECK (kind IN ('backup', 'discovery', 'metrics'));

ALTER TABLE backups DROP CONSTRAINT backups_state_check;
ALTER TABLE backups ADD CONSTRAINT backups_state_check
    CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled', 'expired'));

ALTER TABLE schedules
    DROP COLUMN IF EXISTS expected_grace_minutes,
    DROP COLUMN IF EXISTS expected_cron;

DROP INDEX IF EXISTS idx_backups_instance_completed;
DROP INDEX IF EXISTS idx_backups_external_identity;

ALTER TABLE backups
    DROP COLUMN IF EXISTS observed_at,
    DROP COLUMN IF EXISTS evidence,
    DROP COLUMN IF EXISTS external_location,
    DROP COLUMN IF EXISTS external_id,
    DROP COLUMN IF EXISTS origin;
