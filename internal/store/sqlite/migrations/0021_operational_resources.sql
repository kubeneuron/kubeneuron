-- 0021 — durable v0.4.0 operational product resources.
--
-- Every resource body is normalized JSON behind a versioned envelope.  The
-- envelope makes lifecycle updates optimistic, scopes every query by
-- tenant/cluster, binds request idempotency, and preserves an append-only
-- hash-chained audit record without forcing unrelated controller workflow
-- tables to learn each new console resource shape.

CREATE TABLE IF NOT EXISTS operational_resources (
    kind          TEXT NOT NULL,
    id            TEXT NOT NULL,
    state         TEXT NOT NULL,
    tenant        TEXT NOT NULL DEFAULT '',
    cluster       TEXT NOT NULL DEFAULT '',
    actor         TEXT NOT NULL DEFAULT '',
    config_digest TEXT NOT NULL DEFAULT '',
    payload       TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    expires_at    TEXT,
    version       INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (kind, id)
);
CREATE INDEX IF NOT EXISTS idx_operational_resources_list
    ON operational_resources (kind, state, tenant, cluster, created_at, id);
CREATE INDEX IF NOT EXISTS idx_operational_resources_expiry
    ON operational_resources (expires_at);

CREATE TABLE IF NOT EXISTS operational_idempotency (
    kind            TEXT NOT NULL,
    actor           TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    request_digest  TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    expires_at      TEXT NOT NULL,
    PRIMARY KEY (kind, actor, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_operational_idempotency_expiry
    ON operational_idempotency (expires_at);

CREATE TABLE IF NOT EXISTS operational_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    kind        TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    time        TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    request_id  TEXT NOT NULL DEFAULT '',
    decision_id TEXT NOT NULL DEFAULT '',
    params      TEXT NOT NULL DEFAULT '{}',
    result      TEXT NOT NULL DEFAULT '',
    prev_hash   TEXT NOT NULL DEFAULT '',
    hash        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_operational_audit_resource
    ON operational_audit (kind, resource_id, id);
