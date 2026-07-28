-- 0003 — deduplicate at-least-once agent event replays by capture-time ID.
-- Empty IDs (events from agents predating the field) are exempt.

ALTER TABLE events ADD COLUMN event_id TEXT NOT NULL DEFAULT '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_events_event_id
    ON events (event_id)
    WHERE event_id != '';
