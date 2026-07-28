-- 0004 — durable controller->agent action queue. Agents poll for work over
-- their authenticated channel and post results back; the deterministic
-- action ID makes enqueue and execution idempotent across restarts.

CREATE TABLE IF NOT EXISTS actions (
    id          TEXT PRIMARY KEY,               -- hash(incident, step, attempt)
    node        TEXT NOT NULL,
    incident_id TEXT NOT NULL DEFAULT '',
    type        TEXT NOT NULL,
    params      TEXT NOT NULL DEFAULT '{}',     -- JSON
    timeout_ns  INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'done'
    result      TEXT NOT NULL DEFAULT '',        -- JSON ActionResult when done
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_actions_node_state
    ON actions (node, state, created_at);
