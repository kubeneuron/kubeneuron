-- 0015 — impact previews and the digest an approval binds to (v0.8
-- development). Keep this schema in lockstep with SQLite 0024; see its notes.
CREATE TABLE IF NOT EXISTS impact_previews (
    id                 BIGSERIAL PRIMARY KEY,
    incident_id        TEXT    NOT NULL REFERENCES incidents(id),
    epoch              INTEGER NOT NULL,
    computed_at        TEXT    NOT NULL,
    version            TEXT    NOT NULL,
    digest             TEXT    NOT NULL,
    payload            TEXT,
    unavailable_reason TEXT,
    UNIQUE (incident_id, epoch, computed_at)
);
CREATE INDEX IF NOT EXISTS idx_impact_previews_round ON impact_previews (incident_id, epoch, computed_at);

ALTER TABLE approvals ADD COLUMN IF NOT EXISTS impact_digest TEXT NOT NULL DEFAULT '';
