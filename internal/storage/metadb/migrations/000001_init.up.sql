-- Fleetward metadata schema v1.
--
-- Two rules hold across every table in this file:
--
--   1. Every tenant-scoped table carries tenant_id, from the very first migration. Multi-tenancy is
--      not a Phase 2 feature to be retrofitted; adding this column later would mean a full-schema
--      migration plus an audit of every query (ADR-0008).
--   2. Credentials are never stored in plaintext. The connections table holds only a reference into
--      the secrets table, whose payload is an AES-GCM envelope (ADR-0009).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- -----------------------------------------------------------------------------------------------
-- Tenancy and identity
-- -----------------------------------------------------------------------------------------------

CREATE TABLE tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    is_active   BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE tenants IS
    'Top-level isolation boundary. The MVP runs single-tenant, but the column exists everywhere.';

-- Users are provisioned from OIDC claims on first login; Fleetward stores no passwords (ADR-0008).
CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    subject       TEXT        NOT NULL,
    email         TEXT        NOT NULL,
    display_name  TEXT        NOT NULL DEFAULT '',
    is_active     BOOLEAN     NOT NULL DEFAULT TRUE,
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The OIDC subject is unique per tenant, not globally: the same person may exist in two
    -- tenants backed by different identity providers.
    UNIQUE (tenant_id, subject)
);

CREATE INDEX idx_users_tenant_email ON users (tenant_id, email);

-- -----------------------------------------------------------------------------------------------
-- Estate
-- -----------------------------------------------------------------------------------------------

CREATE TABLE environments (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name          TEXT        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    -- Production environments require stronger confirmation before destructive operations.
    is_production BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, name)
);

CREATE TABLE instances (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    environment_id UUID        NOT NULL REFERENCES environments (id) ON DELETE RESTRICT,
    name           TEXT        NOT NULL,
    -- Canonical engine identifier, e.g. 'postgresql'. Used to route to a plugin and for display.
    -- Core must never branch on this value; it branches on the plugin's declared capabilities.
    engine_type    TEXT        NOT NULL,
    engine_version TEXT        NOT NULL DEFAULT '',
    host           TEXT        NOT NULL,
    port           INTEGER     NOT NULL CHECK (port > 0 AND port <= 65535),
    labels         JSONB       NOT NULL DEFAULT '{}'::JSONB,
    -- Latest health state, mirrored from fleetward.v1.HealthState.
    health         TEXT        NOT NULL DEFAULT 'HEALTH_STATE_UNKNOWN',
    health_message TEXT        NOT NULL DEFAULT '',
    last_seen_at   TIMESTAMPTZ,
    -- Cached Discover output, refreshed on a schedule and on demand.
    discovery      JSONB       NOT NULL DEFAULT '{}'::JSONB,
    is_active      BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, environment_id, name)
);

CREATE INDEX idx_instances_tenant ON instances (tenant_id) WHERE is_active;
CREATE INDEX idx_instances_environment ON instances (environment_id);
CREATE INDEX idx_instances_engine_type ON instances (tenant_id, engine_type);
CREATE INDEX idx_instances_labels ON instances USING GIN (labels);

-- Ciphertext store for the AES-GCM secrets provider (ADR-0009).
--
-- The payload is an opaque envelope: a data key wrapped by the master key, plus the encrypted
-- value, with (tenant_id, name) bound in as additional authenticated data. Moving a row between
-- tenants therefore makes it undecryptable rather than readable by the wrong party.
CREATE TABLE secrets (
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    ciphertext  BYTEA       NOT NULL,
    key_version INTEGER     NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (tenant_id, name)
);

COMMENT ON COLUMN secrets.ciphertext IS
    'AES-GCM envelope. Never logged, never returned by any read API, never sent to a plugin.';

CREATE TABLE connections (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    instance_id UUID        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    username    TEXT        NOT NULL,
    database    TEXT        NOT NULL DEFAULT '',
    -- Reference into secrets.name for this connection's password and client key material.
    secret_name TEXT        NOT NULL,
    tls_enabled BOOLEAN     NOT NULL DEFAULT FALSE,
    options     JSONB       NOT NULL DEFAULT '{}'::JSONB,
    is_default  BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_connections_instance ON connections (instance_id);

-- Exactly one default connection per instance.
CREATE UNIQUE INDEX idx_connections_one_default
    ON connections (instance_id) WHERE is_default;

-- -----------------------------------------------------------------------------------------------
-- Authorization
-- -----------------------------------------------------------------------------------------------

-- Roles are ordered: viewer < operator < dba < admin. The rank column makes "at least dba" a
-- comparison rather than a hand-maintained list in application code (ADR-0008).
CREATE TABLE roles (
    name        TEXT PRIMARY KEY,
    rank        INTEGER NOT NULL UNIQUE,
    description TEXT    NOT NULL
);

INSERT INTO roles (name, rank, description) VALUES
    ('viewer',   10, 'Read-only access to inventory, health, backups, and alerts.'),
    ('operator', 20, 'May acknowledge alerts and trigger discovery. Cannot back up or restore.'),
    ('dba',      30, 'May run backups, verifications, and restores within the granted scope.'),
    ('admin',    40, 'Full control including user, role, and instance administration.');

-- A grant binds a user to a role within a scope. Scope is environment or instance; a NULL in both
-- columns means the grant covers the whole tenant.
CREATE TABLE role_grants (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id        UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_name      TEXT        NOT NULL REFERENCES roles (name) ON DELETE RESTRICT,
    environment_id UUID REFERENCES environments (id) ON DELETE CASCADE,
    instance_id    UUID REFERENCES instances (id) ON DELETE CASCADE,
    granted_by     UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A grant is scoped to at most one level, never both at once.
    CONSTRAINT role_grants_single_scope
        CHECK (environment_id IS NULL OR instance_id IS NULL)
);

CREATE INDEX idx_role_grants_user ON role_grants (user_id);
CREATE INDEX idx_role_grants_scope ON role_grants (tenant_id, environment_id, instance_id);

-- -----------------------------------------------------------------------------------------------
-- Scheduling
-- -----------------------------------------------------------------------------------------------

-- A schedule is a recurring intent; jobs are its individual runs.
CREATE TABLE schedules (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    instance_id       UUID        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    kind              TEXT        NOT NULL CHECK (kind IN ('backup', 'discovery', 'metrics')),
    cron_expression   TEXT        NOT NULL,
    timezone          TEXT        NOT NULL DEFAULT 'UTC',
    -- Backup method and options, interpreted by the plugin serving this instance.
    method_id         TEXT        NOT NULL DEFAULT '',
    options           JSONB       NOT NULL DEFAULT '{}'::JSONB,
    -- Verification policy: always, sampled, or manual. 'sampled' uses verify_sample_percent.
    verify_policy     TEXT        NOT NULL DEFAULT 'always'
                                  CHECK (verify_policy IN ('always', 'sampled', 'manual')),
    verify_sample_percent INTEGER NOT NULL DEFAULT 100
                                  CHECK (verify_sample_percent BETWEEN 0 AND 100),
    retention_days    INTEGER     NOT NULL DEFAULT 30 CHECK (retention_days > 0),
    is_enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    next_run_at       TIMESTAMPTZ,
    last_run_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_schedules_due ON schedules (next_run_at) WHERE is_enabled;
CREATE INDEX idx_schedules_instance ON schedules (instance_id);

-- Jobs are the unit of work the scheduler leases and runs (ADR-0013).
CREATE TABLE jobs (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    schedule_id    UUID REFERENCES schedules (id) ON DELETE SET NULL,
    instance_id    UUID        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    kind           TEXT        NOT NULL CHECK (kind IN ('backup', 'verify', 'restore', 'discovery', 'metrics')),
    state          TEXT        NOT NULL DEFAULT 'pending'
                               CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled')),
    payload        JSONB       NOT NULL DEFAULT '{}'::JSONB,
    scheduled_for  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Lease fields implement at-most-once execution. A runner claims a job by setting lease_owner
    -- and lease_expires_at in one atomic UPDATE, then renews lease_expires_at on a heartbeat while
    -- the job runs. A crashed runner's lease simply expires and the job becomes claimable again.
    lease_owner       TEXT,
    lease_expires_at  TIMESTAMPTZ,
    heartbeat_at      TIMESTAMPTZ,

    attempts       INTEGER     NOT NULL DEFAULT 0,
    max_attempts   INTEGER     NOT NULL DEFAULT 3,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    error_message  TEXT        NOT NULL DEFAULT '',
    triggered_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The claim query's covering index: find pending work that is due and unleased.
CREATE INDEX idx_jobs_claimable
    ON jobs (scheduled_for, lease_expires_at)
    WHERE state IN ('pending', 'running');

CREATE INDEX idx_jobs_instance ON jobs (instance_id, created_at DESC);
CREATE INDEX idx_jobs_state ON jobs (tenant_id, state);

-- At most one active job of a given kind per instance. This is the database-level backstop behind
-- lease claiming: two concurrent pg_basebackup runs against one production instance is an
-- operational incident, so it is prevented by a constraint rather than by careful code.
CREATE UNIQUE INDEX idx_jobs_one_active_per_instance_kind
    ON jobs (instance_id, kind)
    WHERE state IN ('pending', 'running');

-- -----------------------------------------------------------------------------------------------
-- Backups and verification
-- -----------------------------------------------------------------------------------------------

CREATE TABLE backups (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    instance_id        UUID        NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    job_id             UUID REFERENCES jobs (id) ON DELETE SET NULL,
    schedule_id        UUID REFERENCES schedules (id) ON DELETE SET NULL,
    method_id          TEXT        NOT NULL,
    state              TEXT        NOT NULL DEFAULT 'pending'
                                   CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled', 'expired')),
    -- Object storage location. Kept even after expiry so an audit can show what once existed.
    bucket             TEXT        NOT NULL DEFAULT '',
    object_key         TEXT        NOT NULL DEFAULT '',
    size_bytes         BIGINT      NOT NULL DEFAULT 0,
    checksum_algorithm TEXT        NOT NULL DEFAULT '',
    checksum_value     TEXT        NOT NULL DEFAULT '',
    engine_version     TEXT        NOT NULL DEFAULT '',
    -- Point in time the artifact restores to, which is not the same as when the job finished.
    consistency_point  TIMESTAMPTZ,
    -- Ground truth captured at backup time; verification compares the restored instance to it.
    -- Without a manifest, verification degrades to "did it start", which is not verification.
    manifest           JSONB       NOT NULL DEFAULT '{}'::JSONB,
    -- WAL segments, incremental chain members, and other companion objects.
    additional_artifacts JSONB     NOT NULL DEFAULT '[]'::JSONB,
    metadata           JSONB       NOT NULL DEFAULT '{}'::JSONB,
    started_at         TIMESTAMPTZ,
    completed_at       TIMESTAMPTZ,
    duration_ms        BIGINT      NOT NULL DEFAULT 0,
    expires_at         TIMESTAMPTZ,
    error_message      TEXT        NOT NULL DEFAULT '',
    triggered_manually BOOLEAN     NOT NULL DEFAULT FALSE,
    triggered_by       UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_backups_instance_time ON backups (instance_id, created_at DESC);
CREATE INDEX idx_backups_tenant_state ON backups (tenant_id, state);
CREATE INDEX idx_backups_expiring ON backups (expires_at) WHERE state = 'succeeded';

CREATE TABLE verifications (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    backup_id      UUID        NOT NULL REFERENCES backups (id) ON DELETE CASCADE,
    job_id         UUID REFERENCES jobs (id) ON DELETE SET NULL,
    -- Mirrors fleetward.v1.VerificationStatus. 'inconclusive' is deliberately distinct from
    -- 'failed': a sandbox that never became ready is an infrastructure problem, and reporting it
    -- as data loss would train operators to ignore the alert that matters.
    status         TEXT        NOT NULL DEFAULT 'pending'
                               CHECK (status IN ('pending', 'running', 'verified', 'failed', 'inconclusive')),
    checks         JSONB       NOT NULL DEFAULT '[]'::JSONB,
    report         TEXT        NOT NULL DEFAULT '',
    sandbox_id     TEXT        NOT NULL DEFAULT '',
    sandbox_image  TEXT        NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    duration_ms    BIGINT      NOT NULL DEFAULT 0,
    error_message  TEXT        NOT NULL DEFAULT '',
    triggered_by   UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_verifications_backup ON verifications (backup_id, created_at DESC);

-- The estate grid's most important query: which backups are believed good but proved bad.
CREATE INDEX idx_verifications_failed
    ON verifications (tenant_id, completed_at DESC)
    WHERE status = 'failed';

CREATE TABLE restores (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    backup_id       UUID        NOT NULL REFERENCES backups (id) ON DELETE RESTRICT,
    job_id          UUID REFERENCES jobs (id) ON DELETE SET NULL,
    -- 'sandbox' restores are safe by construction; 'instance' restores are destructive and require
    -- the dba role plus typed confirmation.
    target_kind     TEXT        NOT NULL CHECK (target_kind IN ('sandbox', 'instance')),
    target_instance_id UUID REFERENCES instances (id) ON DELETE SET NULL,
    point_in_time   TIMESTAMPTZ,
    recovered_to    TIMESTAMPTZ,
    state           TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'canceled')),
    started_at      TIMESTAMPTZ,
    completed_at    TIMESTAMPTZ,
    duration_ms     BIGINT      NOT NULL DEFAULT 0,
    error_message   TEXT        NOT NULL DEFAULT '',
    triggered_by    UUID REFERENCES users (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_restores_backup ON restores (backup_id);

-- -----------------------------------------------------------------------------------------------
-- Alerting and events
-- -----------------------------------------------------------------------------------------------

CREATE TABLE alert_rules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    -- Rule kinds map to capability-gated evaluators, never to engine names.
    kind           TEXT        NOT NULL CHECK (kind IN (
                       'instance_down', 'verification_failed', 'backup_failed',
                       'backup_missing', 'storage_threshold', 'replication_lag', 'custom_promql')),
    severity       TEXT        NOT NULL DEFAULT 'warning'
                               CHECK (severity IN ('info', 'warning', 'critical')),
    expression     TEXT        NOT NULL DEFAULT '',
    threshold      DOUBLE PRECISION,
    for_duration_s INTEGER     NOT NULL DEFAULT 0,
    -- NULL scope columns mean the rule applies tenant-wide.
    environment_id UUID REFERENCES environments (id) ON DELETE CASCADE,
    instance_id    UUID REFERENCES instances (id) ON DELETE CASCADE,
    is_enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, name)
);

CREATE TABLE alerts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    rule_id        UUID REFERENCES alert_rules (id) ON DELETE SET NULL,
    instance_id    UUID REFERENCES instances (id) ON DELETE CASCADE,
    severity       TEXT        NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    state          TEXT        NOT NULL DEFAULT 'firing'
                               CHECK (state IN ('firing', 'acknowledged', 'resolved')),
    summary        TEXT        NOT NULL,
    detail         TEXT        NOT NULL DEFAULT '',
    labels         JSONB       NOT NULL DEFAULT '{}'::JSONB,
    -- Deduplication key: a rule that keeps firing updates one row rather than creating thousands.
    fingerprint    TEXT        NOT NULL,
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by UUID REFERENCES users (id) ON DELETE SET NULL,
    resolved_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_alerts_active_fingerprint
    ON alerts (tenant_id, fingerprint)
    WHERE state <> 'resolved';

CREATE INDEX idx_alerts_open ON alerts (tenant_id, severity, started_at DESC)
    WHERE state <> 'resolved';

CREATE TABLE notifiers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    kind        TEXT        NOT NULL CHECK (kind IN ('webhook', 'smtp')),
    -- Non-secret settings only. Webhook tokens and SMTP passwords live in the secrets table.
    settings    JSONB       NOT NULL DEFAULT '{}'::JSONB,
    secret_name TEXT        NOT NULL DEFAULT '',
    min_severity TEXT       NOT NULL DEFAULT 'warning'
                            CHECK (min_severity IN ('info', 'warning', 'critical')),
    is_enabled  BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, name)
);

-- Health transitions and other notable occurrences, distinct from alerts: an event is a fact that
-- happened, an alert is a condition that persists.
CREATE TABLE events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    instance_id UUID REFERENCES instances (id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL,
    severity    TEXT        NOT NULL DEFAULT 'info'
                            CHECK (severity IN ('info', 'warning', 'critical')),
    message     TEXT        NOT NULL,
    details     JSONB       NOT NULL DEFAULT '{}'::JSONB,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_events_instance_time ON events (instance_id, occurred_at DESC);
CREATE INDEX idx_events_tenant_time ON events (tenant_id, occurred_at DESC);

-- -----------------------------------------------------------------------------------------------
-- Audit
-- -----------------------------------------------------------------------------------------------

-- Every mutating action lands here. The table is append-only by policy and by trigger: an audit
-- log that can be edited is not evidence.
CREATE TABLE audit_log (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   UUID        NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    user_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    -- Retained separately from user_id so the record survives the user being deleted.
    actor       TEXT        NOT NULL,
    action      TEXT        NOT NULL,
    resource_type TEXT      NOT NULL,
    resource_id TEXT        NOT NULL DEFAULT '',
    -- Never contains credentials, only which fields changed.
    details     JSONB       NOT NULL DEFAULT '{}'::JSONB,
    source_ip   INET,
    user_agent  TEXT        NOT NULL DEFAULT '',
    request_id  TEXT        NOT NULL DEFAULT '',
    succeeded   BOOLEAN     NOT NULL DEFAULT TRUE,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_tenant_time ON audit_log (tenant_id, occurred_at DESC);
CREATE INDEX idx_audit_resource ON audit_log (resource_type, resource_id, occurred_at DESC);
CREATE INDEX idx_audit_user ON audit_log (user_id, occurred_at DESC);

CREATE OR REPLACE FUNCTION audit_log_is_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_is_append_only();

-- -----------------------------------------------------------------------------------------------
-- Shared triggers
-- -----------------------------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY[
        'tenants', 'users', 'environments', 'instances', 'secrets', 'connections',
        'schedules', 'jobs', 'backups', 'verifications', 'restores',
        'alert_rules', 'notifiers'
    ] LOOP
        EXECUTE format(
            'CREATE TRIGGER %I_set_updated_at BEFORE UPDATE ON %I
             FOR EACH ROW EXECUTE FUNCTION set_updated_at()', t, t);
    END LOOP;
END;
$$;

-- -----------------------------------------------------------------------------------------------
-- Bootstrap tenant
-- -----------------------------------------------------------------------------------------------

-- The MVP runs single-tenant. Seeding a default tenant means every table's tenant_id is populated
-- from the first row written, so the multi-tenant query paths are exercised from day one rather
-- than lying dormant until someone enables tenancy.
INSERT INTO tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default');
