-- Reverses 000005_alerts.up.sql.
--
-- Narrowing the kind CHECK back means rows using the widened value have to go first, which is the
-- shape 000002's reversal already established: a constraint cannot be narrowed underneath data that
-- violates it. Every `retention_blocked` rule and every alert those rules produced is deleted.
--
-- Alerts of the other kinds are left exactly as they are. They are the record of conditions that
-- were true, and a down migration that rewrote them would be manufacturing history. Nothing
-- evaluates them after this runs, so they simply stop moving — which is honest.

DELETE FROM alerts
WHERE  rule_id IN (SELECT id FROM alert_rules WHERE kind = 'retention_blocked');

DELETE FROM alert_rules WHERE kind = 'retention_blocked';

ALTER TABLE alert_rules DROP CONSTRAINT IF EXISTS alert_rules_kind_check;

ALTER TABLE alert_rules ADD CONSTRAINT alert_rules_kind_check CHECK (kind IN (
    'instance_down', 'verification_failed', 'backup_failed',
    'backup_missing', 'storage_threshold', 'replication_lag', 'custom_promql'));

DROP INDEX IF EXISTS idx_alert_rules_enabled;

ALTER TABLE notifiers DROP COLUMN IF EXISTS last_attempt_at;
ALTER TABLE notifiers DROP COLUMN IF EXISTS last_success_at;
ALTER TABLE notifiers DROP COLUMN IF EXISTS last_error;

-- The three seeded rules go with the migration that created them. They were configuration this
-- slice supplied rather than anything an operator wrote, and leaving them behind would leave rows
-- nothing evaluates.
DELETE FROM alerts
WHERE  rule_id IN (
    SELECT id FROM alert_rules
    WHERE  tenant_id = '00000000-0000-0000-0000-000000000001'
      AND  name IN ('Backup verification failed', 'Backup window missed', 'Instance down'));

DELETE FROM alert_rules
WHERE  tenant_id = '00000000-0000-0000-0000-000000000001'
  AND  name IN ('Backup verification failed', 'Backup window missed', 'Instance down');
