-- SPDX-License-Identifier: LicenseRef-KubeNeuron

-- 0025 — the result an idempotent operation produced (v0.8 development).
-- An operational idempotency record used to hold only the request digest
-- and the resource it acted on, which is enough to detect a conflicting
-- replay but not to answer a faithful one: a replayed impact-preview
-- recompute returned the round's NEWEST preview, which after a second
-- recompute is somebody else's result. result_digest is the digest of what
-- THIS key produced; a replay returns exactly that. Empty for every row
-- written before this migration and for operations that record no result.
-- Both engines move in lockstep (PostgreSQL 0016).
ALTER TABLE operational_idempotency ADD COLUMN result_digest TEXT NOT NULL DEFAULT '';
