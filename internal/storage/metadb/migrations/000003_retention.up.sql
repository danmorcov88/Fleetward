-- Retention and expiry (ADR-0030, ADR-0031, ADR-0032).
--
-- This is the first migration in service of a feature that deletes. Everything before it was
-- read-only or additive, so the schema's job here is not to make retention possible — `expires_at`,
-- `retention_days`, the `expired` state and `idx_backups_expiring` have all been present since
-- migration 000001 — but to make the ways it could go wrong impossible.
--
-- Three things, and each one closes a specific way a future query could destroy something.

-- -----------------------------------------------------------------------------------------------
-- 1. An observed backup can never be expired
-- -----------------------------------------------------------------------------------------------
--
-- An observed backup is somebody else's file. Fleetward reports on it and must never delete it
-- (ADR-0015), and the whole adoption story rests on that: pointing Fleetward at an estate changes
-- nothing on it.
--
-- The obvious implementation of that promise is `WHERE origin = 'managed'` in the retention query,
-- and the obvious implementation is not good enough. A predicate in a query is a line somebody
-- deletes in six months while refactoring, or forgets to repeat in a second query written by
-- someone who has not read ADR-0015 — and the consequence of forgetting it is destroying a
-- customer's own backups.
--
-- So the state transition itself is made illegal. Retention's only way to act is to set
-- `state = 'expired'`; on an observed row that now raises 23514 and rolls the transaction back.
-- A query that forgets the filter fails loudly instead of succeeding quietly.
--
-- This deliberately forecloses ever using `expired` to mean "the DBA's own retention removed this
-- file". That is a different fact — evidence read from the engine, not an action Fleetward took —
-- and being forced to give it a different word is the point rather than the cost.
ALTER TABLE backups ADD CONSTRAINT backups_observed_never_expires
    CHECK (NOT (origin = 'observed' AND state = 'expired'));

COMMENT ON CONSTRAINT backups_observed_never_expires ON backups IS
    'An observed backup is somebody else''s file and Fleetward never deletes it (ADR-0015). '
    'Enforced here rather than in a WHERE clause so a query that forgets the filter fails.';

-- -----------------------------------------------------------------------------------------------
-- 2. When the artifact went, which is not when the backup expired
-- -----------------------------------------------------------------------------------------------
--
-- A backup is a row and an object, and there is no transaction across both. The order is settled in
-- ADR-0030: the row moves 'succeeded' -> 'expired' and commits on its own, and only then is the
-- object deleted. A control plane killed between the two leaves a row that is `expired` with its
-- artifact still in the bucket.
--
-- This column is what makes that state distinguishable from a finished one, and therefore what
-- makes the leftover self-reconciling: the next sweep — on any control plane, not necessarily the
-- one that died — selects exactly `expired AND artifact_deleted_at IS NULL AND object_key <> ''`
-- and finishes the job. Deleting an object that is already gone is not an error, so the re-run is
-- free.
--
-- `bucket` and `object_key` are deliberately left intact afterwards. The row is the audit record of
-- what once existed; only the bytes go.
ALTER TABLE backups ADD COLUMN artifact_deleted_at TIMESTAMPTZ;

COMMENT ON COLUMN backups.artifact_deleted_at IS
    'When retention deleted this backup''s artifact from object storage. NULL on a row whose '
    'artifact is still there, which is what the sweep''s second pass selects on (ADR-0030).';

-- The delete queue. Small by construction — a row leaves this index the moment its object is gone —
-- so the sweep's second statement is an index scan over the handful of artifacts still to remove
-- rather than a scan of every backup ever taken.
CREATE INDEX idx_backups_artifact_pending_delete
    ON backups (updated_at)
    WHERE state = 'expired' AND artifact_deleted_at IS NULL AND object_key <> '';

-- -----------------------------------------------------------------------------------------------
-- 3. What this migration deliberately does NOT do
-- -----------------------------------------------------------------------------------------------
--
-- It does not backfill `expires_at`, and that omission is the single most important line in the
-- file.
--
-- An expiry is stamped when a backup is taken, from the retention its schedule declared at that
-- moment (ADR-0031). Every backup taken before this migration therefore has `expires_at IS NULL`,
-- NULL means "never expires", and the first sweep after an upgrade deletes exactly nothing. An
-- operator gets a full retention period of warning, and can watch `fleetward-cli backup retention`
-- fill up before anything goes.
--
-- Computing an expiry from the schedule instead would have been retroactive by construction: the
-- first tick after `docker compose up` would have deleted a year of history from an estate whose
-- owner had not asked for anything.
