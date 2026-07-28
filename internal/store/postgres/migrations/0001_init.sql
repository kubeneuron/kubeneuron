-- 0001_init.sql — complete KubeNeuron schema, PostgreSQL dialect.
--
-- PostgreSQL deployments have no legacy databases to baseline, so this one
-- migration is the consolidated equivalent of the SQLite series 0001..0010.
-- Timestamps are RFC3339 TEXT (matching the shared sqlcore ts() helper);
-- nanosecond and generation counters are BIGINT.

CREATE TABLE IF NOT EXISTS incidents (
    id               TEXT PRIMARY KEY,
    node             TEXT NOT NULL,
    gpu_uuid         TEXT NOT NULL DEFAULT '',
    gpu_index        INTEGER NOT NULL DEFAULT 0,
    class            TEXT NOT NULL,
    state            TEXT NOT NULL,
    playbook         TEXT NOT NULL DEFAULT '',
    step_index       INTEGER NOT NULL DEFAULT 0,
    attempt          INTEGER NOT NULL DEFAULT 0,
    dry_run          INTEGER NOT NULL DEFAULT 0,
    signals_seen     INTEGER NOT NULL DEFAULT 0,
    opened_at        TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    resolved_at      TEXT,
    state_changed_at TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open
    ON incidents (node, gpu_uuid, class)
    WHERE state NOT IN ('RESOLVED', 'EXPIRED');
CREATE INDEX IF NOT EXISTS idx_incidents_state ON incidents (state);
CREATE INDEX IF NOT EXISTS idx_incidents_node  ON incidents (node);

CREATE TABLE IF NOT EXISTS audit_log (
    id          BIGSERIAL PRIMARY KEY,
    incident_id TEXT NOT NULL REFERENCES incidents (id),
    time        TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '{}',
    result      TEXT NOT NULL DEFAULT '',
    dry_run     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_audit_incident ON audit_log (incident_id);

CREATE TABLE IF NOT EXISTS approvals (
    id          BIGSERIAL PRIMARY KEY,
    incident_id TEXT NOT NULL REFERENCES incidents (id),
    step_name   TEXT NOT NULL,
    decision    TEXT NOT NULL,
    actor       TEXT NOT NULL,
    channel     TEXT NOT NULL,
    at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    name            TEXT PRIMARY KEY,
    platform        TEXT NOT NULL,
    labels          TEXT NOT NULL DEFAULT '{}',
    ssh_addr        TEXT NOT NULL DEFAULT '',
    bmc_addr        TEXT NOT NULL DEFAULT '',
    gpus            TEXT NOT NULL DEFAULT '[]',
    boot_id         TEXT NOT NULL DEFAULT '',
    paused          INTEGER NOT NULL DEFAULT 0,
    agent_last_seen TEXT,
    node_uid        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS events (
    id        BIGSERIAL PRIMARY KEY,
    node      TEXT NOT NULL,
    gpu_index INTEGER NOT NULL DEFAULT 0,
    gpu_uuid  TEXT NOT NULL DEFAULT '',
    xid       INTEGER NOT NULL,
    raw       TEXT NOT NULL DEFAULT '',
    timestamp TEXT NOT NULL,
    event_id  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_events_node_time ON events (node, timestamp);
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_event_id
    ON events (event_id)
    WHERE event_id != '';

CREATE TABLE IF NOT EXISTS config_versions (
    id         BIGSERIAL PRIMARY KEY,
    kind       TEXT NOT NULL,
    version    INTEGER NOT NULL,
    content    TEXT NOT NULL,
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (kind, version)
);

CREATE TABLE IF NOT EXISTS actions (
    id                  TEXT PRIMARY KEY,
    node                TEXT NOT NULL,
    incident_id         TEXT NOT NULL DEFAULT '',
    type                TEXT NOT NULL,
    params              TEXT NOT NULL DEFAULT '{}',
    timeout_ns          BIGINT NOT NULL DEFAULT 0,
    state               TEXT NOT NULL DEFAULT 'pending',
    result              TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    lease_token         TEXT NOT NULL DEFAULT '',
    lease_expires_at_ns BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_actions_node_state
    ON actions (node, state, created_at);
CREATE INDEX IF NOT EXISTS idx_actions_node_lease
    ON actions (node, state, lease_expires_at_ns, created_at, id);

CREATE TABLE IF NOT EXISTS event_outbox (
    id                  BIGSERIAL PRIMARY KEY,
    event_row_id        BIGINT NOT NULL UNIQUE REFERENCES events (id),
    state               TEXT NOT NULL DEFAULT 'pending',
    attempts            INTEGER NOT NULL DEFAULT 0,
    lease_owner         TEXT NOT NULL DEFAULT '',
    lease_token         TEXT NOT NULL DEFAULT '',
    lease_expires_at_ns BIGINT NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    completed_at        TEXT
);
CREATE INDEX IF NOT EXISTS idx_event_outbox_claim
    ON event_outbox (state, lease_expires_at_ns, created_at, id);

CREATE TABLE IF NOT EXISTS accelerator_reports (
    node               TEXT NOT NULL,
    vendor             TEXT NOT NULL,
    observed_at_ns     BIGINT NOT NULL,
    profile_digest     TEXT NOT NULL DEFAULT '',
    readiness          TEXT NOT NULL,
    reasons_json       TEXT NOT NULL DEFAULT '[]',
    devices_json       TEXT NOT NULL DEFAULT '[]',
    driver_version     TEXT NOT NULL DEFAULT '',
    runtime_version    TEXT NOT NULL DEFAULT '',
    topology_safety    TEXT NOT NULL,
    capabilities_json  TEXT NOT NULL DEFAULT '[]',
    profile_uid        TEXT NOT NULL DEFAULT '',
    profile_generation BIGINT NOT NULL DEFAULT 0,
    node_uid           TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (node, vendor)
);
CREATE INDEX IF NOT EXISTS idx_accelerator_reports_node
    ON accelerator_reports (node, vendor);

CREATE TABLE IF NOT EXISTS safety_state (
    kind       TEXT PRIMARY KEY,
    payload    TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
