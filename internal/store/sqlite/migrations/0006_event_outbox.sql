-- 0006 — durable event outbox. Event archival and pending workflow delivery
-- are committed together by ArchiveAndEnqueueEvent. event_row_id is unique so
-- one archived row can result in at most one workflow item.

CREATE TABLE IF NOT EXISTS event_outbox (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    event_row_id        INTEGER NOT NULL UNIQUE REFERENCES events (id),
    state               TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'leased' | 'done' | 'dead'
    attempts            INTEGER NOT NULL DEFAULT 0,
    lease_owner         TEXT NOT NULL DEFAULT '',
    lease_token         TEXT NOT NULL DEFAULT '',
    lease_expires_at_ns INTEGER NOT NULL DEFAULT 0,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    completed_at        TEXT
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_claim
    ON event_outbox (state, lease_expires_at_ns, created_at, id);
