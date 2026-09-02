-- Observed backups: backups Fleetward did not take (ADR-0015, ADR-0027, ADR-0028).
--
-- The estate this product exists for already backs itself up, by cron and by scripts that predate
-- Fleetward. Reporting on those without changing anything about them is what makes adoption
-- possible, and it needs three things from the schema: an origin on every backup, an identity that
-- came from the engine so a poll cannot insert the same backup twice, and somewhere to record what
-- a backup is supposed to look like so a gap is computable.
--
-- Observed backups live in `backups` rather than a table of their own on purpose. Adherence asks
-- one question — did a backup happen inside the window — and it does not care who took it, so
-- splitting the two origins would turn every such query into a UNION and every future query into an
-- opportunity to forget one half.

-- -----------------------------------------------------------------------------------------------
-- Origin
-- -----------------------------------------------------------------------------------------------

ALTER TABLE backups
    -- 'managed' is the default so every row that already exists keeps the meaning it was written
    -- with: Fleetward took it, captured a manifest, and controls the artifact.
    ADD COLUMN origin TEXT NOT NULL DEFAULT 'managed'
        CHECK (origin IN ('managed', 'observed')),
    -- The identity the engine gave this backup, in the engine's own vocabulary. Opaque to core,
    -- which upserts on it and never parses it (ADR-0027). NULL where the engine assigns none.
    ADD COLUMN external_id TEXT,
    -- Where the artifact is, as the engine or the filesystem names it. An observed artifact is
    -- somebody else's file: Fleetward reports where it is and never reads, moves, or deletes it.
    -- `bucket` and `object_key` stay empty on an observed row, because they mean "an object
    -- Fleetward owns" and a UI that offered a download of this would be lying.
    ADD COLUMN external_location TEXT NOT NULL DEFAULT '',
    -- What the evidence this row was read from could and could not establish. Carried per row
    -- rather than looked up from the plugin, so a backup observed a year ago still says what was
    -- known about it then.
    ADD COLUMN evidence JSONB NOT NULL DEFAULT '{}'::JSONB,
    -- When Fleetward first saw this evidence, which is not when the backup happened.
    ADD COLUMN observed_at TIMESTAMPTZ;

COMMENT ON COLUMN backups.origin IS
    'managed = Fleetward ran it and can verify it; observed = evidence about somebody else''s '
    'backup, which carries no manifest and therefore can never be verified (ADR-0015).';

-- A backup observed twice is one backup. This is the whole anti-duplication story, and it covers
-- managed rows too: a plugin that knows what the engine called the backup it just took records it
-- here, so the next observation poll upserts onto that row instead of inserting the same physical
-- backup a second time under an origin it does not have (ADR-0027).
CREATE UNIQUE INDEX idx_backups_external_identity
    ON backups (instance_id, external_id)
    WHERE external_id IS NOT NULL;

-- Adherence asks "was there a backup between these two instants", per instance, over both origins.
-- idx_backups_instance_time is on created_at, which for an observed backup is when Fleetward
-- happened to poll rather than when the backup ran.
CREATE INDEX idx_backups_instance_completed ON backups (instance_id, completed_at DESC);

-- A file exists and nothing about it says the dump that wrote it finished. That is a real answer
-- and it is neither success nor failure, so it gets its own state rather than being rounded into
-- one of them. Only ever written for an observed row.
ALTER TABLE backups DROP CONSTRAINT backups_state_check;
ALTER TABLE backups ADD CONSTRAINT backups_state_check
    CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled', 'expired', 'unknown'));

-- -----------------------------------------------------------------------------------------------
-- Observation as scheduled work
-- -----------------------------------------------------------------------------------------------
--
-- Both constraints, not one: a schedule kind that materializes into a job kind needs the job side
-- widened too, or the scheduler creates schedules it can never run (ADR-0028).

ALTER TABLE schedules DROP CONSTRAINT schedules_kind_check;
ALTER TABLE schedules ADD CONSTRAINT schedules_kind_check
    CHECK (kind IN ('backup', 'discovery', 'metrics', 'observe'));

ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check
    CHECK (kind IN ('backup', 'verify', 'restore', 'discovery', 'metrics', 'observe'));

-- -----------------------------------------------------------------------------------------------
-- The declaration half of "declare what should be true, detect what actually is"
-- -----------------------------------------------------------------------------------------------
--
-- Two cron expressions on one row, and they answer different questions. cron_expression is how
-- often Fleetward goes and looks; expected_cron is how often a backup is supposed to have happened.
-- Deriving the second from the first would report "we polled and saw nothing", and deriving it from
-- the observed rhythm would answer "is this normal for you" instead of "is this what you asked
-- for". The second question is the product.
ALTER TABLE schedules
    ADD COLUMN expected_cron TEXT NOT NULL DEFAULT '',
    -- How late a backup may be and still count. Absorbs a run that started on time and took longer
    -- than usual, and absorbs the one hour an engine that records local time without an offset can
    -- be wrong by across a daylight-saving transition.
    ADD COLUMN expected_grace_minutes INTEGER NOT NULL DEFAULT 0
        CHECK (expected_grace_minutes >= 0);

COMMENT ON COLUMN schedules.expected_cron IS
    'When a backup of this instance is supposed to have happened, interpreted in `timezone`. '
    'Empty means nothing was declared, and adherence reports that rather than guessing.';
