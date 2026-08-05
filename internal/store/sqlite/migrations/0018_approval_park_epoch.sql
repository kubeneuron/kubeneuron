-- 0018 — first-class approval rounds. Each park (and re-park) of an incident
-- is one epoch: the incident carries its current epoch, and every approvals
-- row (the request written at park, the decisions written by humans) is
-- stamped with the epoch it belongs to. The resume path then asks "the
-- decision for THIS park" directly instead of inferring it from row recency
-- and StateChangedAt ordering, and a decision can never be honored across a
-- re-park because its epoch no longer matches.
--
-- Pre-upgrade rows ALL default to epoch 0 — requests and any orphaned
-- decisions from any number of old parks alike — so epoch 0 is not a
-- verifiable round and the controller never consults it as one: an epoch-0
-- park is re-parked into round 1 and decided fresh. Deliberate: better one
-- repeated click than pairing a stale approval with a request from a
-- different park.
ALTER TABLE incidents ADD COLUMN approval_epoch INTEGER NOT NULL DEFAULT 0;
ALTER TABLE approvals ADD COLUMN park_epoch INTEGER NOT NULL DEFAULT 0;
