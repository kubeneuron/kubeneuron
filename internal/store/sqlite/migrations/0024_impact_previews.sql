-- SPDX-License-Identifier: LicenseRef-KubeNeuron

-- 0024 — impact previews and the digest an approval binds to (v0.8
-- development). An impact preview is what a human was shown about the node,
-- its workloads and the disruption downstream when an incident parked for
-- approval. One round may hold several records (a recompute appends; the
-- newest by computed_at is current), and the digest identifies exactly
-- which one was seen: a decision row may carry it, and the controller can
-- refuse a decision whose digest no longer matches the round's current
-- preview. Pre-upgrade decision rows carry an empty digest, which means
-- "no preview was shown", never a match.
--
-- The payload is the canonical JSON document, bounded by the controller; an
-- unavailable preview has no payload and a reason instead. Both engines move
-- in lockstep (PostgreSQL 0015).
CREATE TABLE IF NOT EXISTS impact_previews (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
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

ALTER TABLE approvals ADD COLUMN impact_digest TEXT NOT NULL DEFAULT '';
