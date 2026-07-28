-- 0002 — track when an incident's state last changed, separately from
-- updated_at (which duplicate signals bump). Timeouts that anchor to a state
-- (approval TTL, quiet windows) must use this column.

ALTER TABLE incidents ADD COLUMN state_changed_at TEXT NOT NULL DEFAULT '';
