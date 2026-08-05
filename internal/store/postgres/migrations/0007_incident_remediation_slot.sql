-- 0007 — persist remediation-slot ownership on the incident row (see the
-- SQLite 0016 notes): the failover rebuild reads ownership instead of
-- inferring it from state/step_index, which missed escalated incidents.
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS remediation_slot_held INTEGER NOT NULL DEFAULT 0;

UPDATE incidents SET remediation_slot_held = 1
 WHERE state NOT IN ('RESOLVED','EXPIRED','NEEDS_HUMAN')
   AND (state IN ('EXECUTING','VERIFYING') OR step_index > 0)
   AND remediation_slot_held = 0;
