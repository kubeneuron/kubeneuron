-- 0016 — the result an idempotent operation produced (v0.8 development).
-- Keep this schema in lockstep with SQLite 0025; see its notes.
ALTER TABLE operational_idempotency ADD COLUMN IF NOT EXISTS result_digest TEXT NOT NULL DEFAULT '';
