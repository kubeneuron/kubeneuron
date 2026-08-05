-- 0017 — the tri-state destructive-arming fact a node's agent declares at
-- registration. TEXT with '' = unknown, matching the explicit
-- unknown-vs-observed precedent of holders_json: a row written before this
-- column, or by an agent that never reported, must never read as a declared
-- value in either direction.
ALTER TABLE nodes ADD COLUMN agent_arming TEXT NOT NULL DEFAULT '';
