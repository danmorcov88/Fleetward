-- Alert rules and delivery (ADR-0038, ADR-0039, ADR-0040).
--
-- `alert_rules`, `alerts` and `notifiers` have existed since migration 000001 and have never held
-- a row. Slice B7 writes the code that reads them, and this migration adds only what that code
-- cannot work without.

-- -----------------------------------------------------------------------------------------------
-- 1. One more rule kind
-- -----------------------------------------------------------------------------------------------
--
-- `kind` is a closed set that maps to evaluators. It grows by migration and by nothing else, and it
-- never contains an engine name — an evaluator that branched on `instances.engine_type` would be
-- the violation CLAUDE.md §4.1 exists to forbid.
--
-- `retention_blocked` is added because the condition is real and none of the seven existing kinds
-- describes it. A retention sweep whose object store refuses every delete leaves its backlog in
-- rows — `backups.state = 'expired' AND object_key <> ''` is literally the queue — and until now
-- that was visible only in a log line nobody reads. `storage_threshold` is about storage
-- utilization and would have been the wrong word for it, which matters: a kind is the vocabulary an
-- operator writes rules in.
ALTER TABLE alert_rules DROP CONSTRAINT alert_rules_kind_check;

ALTER TABLE alert_rules ADD CONSTRAINT alert_rules_kind_check CHECK (kind IN (
    'instance_down', 'verification_failed', 'backup_failed',
    'backup_missing', 'storage_threshold', 'replication_lag', 'custom_promql',
    'retention_blocked'));

COMMENT ON COLUMN alert_rules.kind IS
    'Selects an evaluator. A closed set, widened only by migration, and never an engine name '
    '(CLAUDE.md §4.1). Three of the values — storage_threshold, replication_lag and custom_promql — '
    'have no evaluator yet and rule creation refuses them: a rule accepted and never evaluated is '
    'worse than one refused, because the operator believes they are covered.';

-- Evaluation asks for enabled rules of a kind, tenant by tenant, on every pass.
CREATE INDEX idx_alert_rules_enabled ON alert_rules (tenant_id, kind) WHERE is_enabled;

-- -----------------------------------------------------------------------------------------------
-- 2. A notifier that has been failing all week must be visible
-- -----------------------------------------------------------------------------------------------
--
-- Delivery is at-most-once and best-effort: the alert row is the record, and a notification that
-- could not be sent is logged and dropped rather than queued in an outbox this schema does not have
-- (ADR-0039). That is a defensible trade only if the operator can see it happening, and a log line
-- on a server nobody tails is not seeing it.
--
-- So the three facts a person actually needs live on the row: when we last tried, when we last
-- succeeded, and what went wrong. `last_error` holds the transport's message — a status code, a
-- refused connection, an SMTP reply — and never a credential, because nothing that builds it has
-- one to hand.
ALTER TABLE notifiers ADD COLUMN last_attempt_at TIMESTAMPTZ;
ALTER TABLE notifiers ADD COLUMN last_success_at TIMESTAMPTZ;
ALTER TABLE notifiers ADD COLUMN last_error      TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN notifiers.last_error IS
    'The last delivery failure''s message, cleared on success. Never a credential: the value is a '
    'transport error, and the secret never appears in one.';

COMMENT ON COLUMN notifiers.secret_name IS
    'Reference into secrets.name for this notifier''s one credential — a webhook header value or an '
    'SMTP password. Empty means the notifier needs none. No API reads it back.';

-- -----------------------------------------------------------------------------------------------
-- 3. Three rules, so that a fresh installation alerts on something
-- -----------------------------------------------------------------------------------------------
--
-- An installation whose alerting has to be assembled from nothing before it says anything is an
-- installation that never gets alerting. These three are the product's thesis stated as
-- configuration: a backup that will not restore, a window that closed empty, a server that stopped
-- answering.
--
-- No notifier is seeded, so nothing is delivered anywhere until an operator configures one. The
-- rows appear in the API and the CLI and that is all — which is also why seeding them is safe on an
-- upgrade: an existing installation gains rows to read, not a pager at 3am.
--
-- Tenant-wide (both scope columns NULL) and against the tenant seeded by migration 000001. An
-- operator who disagrees disables one in a single command.
INSERT INTO alert_rules (tenant_id, name, description, kind, severity)
VALUES
    ('00000000-0000-0000-0000-000000000001',
     'Backup verification failed',
     'A backup was restored into a sandbox and the evidence says it is not usable. This is the '
     'alert this product exists to send.',
     'verification_failed', 'critical'),
    ('00000000-0000-0000-0000-000000000001',
     'Backup window missed',
     'A backup was expected inside a declared window and none arrived.',
     'backup_missing', 'warning'),
    ('00000000-0000-0000-0000-000000000001',
     'Instance down',
     'A monitored instance stopped answering its health probe.',
     'instance_down', 'warning');

-- -----------------------------------------------------------------------------------------------
-- 4. What this migration deliberately does NOT do
-- -----------------------------------------------------------------------------------------------
--
-- It adds no notifications table. Delivery has no durable outbox, no per-notifier backoff schedule
-- and no dead-letter queue, and ADR-0039 records why: the alert row is the record, the notification
-- is a convenience, and an outbox is a real piece of engineering that is not what stands between
-- this product and being trusted. `docs/ops/alerting.md` says so where an operator will read it.
--
-- It touches `events` not at all. An event is a fact that happened and an alert is a condition that
-- persists; they are different tables because they are different concepts, and writing to both
-- would double the surface for no additional answer.
--
-- It seeds no notifier. A notifier holds a credential belonging to somebody else's system, and
-- there is no such credential to seed.
