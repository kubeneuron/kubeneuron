-- 0001_init.sql — initial KubeNeuron controller schema (SQLite dialect).

CREATE TABLE IF NOT EXISTS incidents (
    id           TEXT PRIMARY KEY,
    node         TEXT NOT NULL,
    gpu_uuid     TEXT NOT NULL DEFAULT '',
    gpu_index    INTEGER NOT NULL DEFAULT 0,
    class        TEXT NOT NULL,
    state        TEXT NOT NULL,
    playbook     TEXT NOT NULL DEFAULT '',
    step_index   INTEGER NOT NULL DEFAULT 0,
    attempt      INTEGER NOT NULL DEFAULT 0,
    dry_run      INTEGER NOT NULL DEFAULT 0,
    signals_seen INTEGER NOT NULL DEFAULT 0,
    opened_at    TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    resolved_at  TEXT
);

-- At most one non-terminal incident per (target, class).
CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open
    ON incidents (node, gpu_uuid, class)
    WHERE state NOT IN ('RESOLVED', 'EXPIRED');

CREATE INDEX IF NOT EXISTS idx_incidents_state ON incidents (state);
CREATE INDEX IF NOT EXISTS idx_incidents_node  ON incidents (node);

CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_id TEXT NOT NULL REFERENCES incidents (id),
    time        TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '{}',   -- JSON
    result      TEXT NOT NULL DEFAULT '',
    dry_run     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_audit_incident ON audit_log (incident_id);

CREATE TABLE IF NOT EXISTS approvals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    incident_id TEXT NOT NULL REFERENCES incidents (id),
    step_name   TEXT NOT NULL,
    decision    TEXT NOT NULL,                -- 'approved' | 'rejected'
    actor       TEXT NOT NULL,
    channel     TEXT NOT NULL,                -- 'slack' | 'cli' | 'web'
    at          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    name            TEXT PRIMARY KEY,
    platform        TEXT NOT NULL,
    labels          TEXT NOT NULL DEFAULT '{}',  -- JSON
    ssh_addr        TEXT NOT NULL DEFAULT '',
    bmc_addr        TEXT NOT NULL DEFAULT '',
    gpus            TEXT NOT NULL DEFAULT '[]',  -- JSON [{index,uuid,model}]
    boot_id         TEXT NOT NULL DEFAULT '',
    paused          INTEGER NOT NULL DEFAULT 0,
    agent_last_seen TEXT
);

-- Raw agent events (default EventSink). For fleet-scale analytics this table
-- can be mirrored to ClickHouse instead.
CREATE TABLE IF NOT EXISTS events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    node      TEXT NOT NULL,
    gpu_index INTEGER NOT NULL DEFAULT 0,
    gpu_uuid  TEXT NOT NULL DEFAULT '',
    xid       INTEGER NOT NULL,
    raw       TEXT NOT NULL DEFAULT '',
    timestamp TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_events_node_time ON events (node, timestamp);

-- Versioned configuration for the Web UI config editor (design.md §Web UI).
-- configs/*.yaml files seed version 1 on first start.
CREATE TABLE IF NOT EXISTS config_versions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,      -- 'policies' | 'playbooks' | 'inventory'
    version    INTEGER NOT NULL,
    content    TEXT NOT NULL,      -- YAML
    author     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    UNIQUE (kind, version)
);
