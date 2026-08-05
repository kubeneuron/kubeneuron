-- 0009 — first-class approval rounds (see the SQLite 0018 notes): the
-- incident carries its current park epoch and every approvals row is stamped
-- with the epoch it belongs to. Pre-upgrade rows all default to epoch 0,
-- which is never consulted as a round; such parks are re-parked into round 1.
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS approval_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS park_epoch INTEGER NOT NULL DEFAULT 0;
