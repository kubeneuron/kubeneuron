-- 0008 — agent-declared destructive arming on the node row (see the SQLite
-- 0017 notes). '' = unknown, never a declared value.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS agent_arming TEXT NOT NULL DEFAULT '';
