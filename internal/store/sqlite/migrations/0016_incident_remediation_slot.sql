-- 0016 — persist remediation-slot ownership on the incident row.
--
-- The safety gate's MaxConcurrentRemediations occupancy was rebuilt after a
-- leader failover by inference (EXECUTING/VERIFYING or step_index > 0), which
-- misses an escalated incident: escalate() resets step_index to 0 in
-- EVALUATING while the incident still holds its slot until it halts. The bit
-- is written atomically with the EXECUTING transition and cleared atomically
-- with the halting transition, so rebuild reads ownership instead of
-- guessing it.
ALTER TABLE incidents ADD COLUMN remediation_slot_held INTEGER NOT NULL DEFAULT 0;

-- One-time backfill reproducing the old inference for rows that predate the
-- bit, so an upgrade does not drop occupancy the old code would have
-- reseeded. (Escalated step_index=0 rows are still missed here — identical
-- to pre-upgrade behavior; they self-heal at their next admitted step.)
UPDATE incidents SET remediation_slot_held = 1
 WHERE state NOT IN ('RESOLVED','EXPIRED','NEEDS_HUMAN')
   AND (state IN ('EXECUTING','VERIFYING') OR step_index > 0);
