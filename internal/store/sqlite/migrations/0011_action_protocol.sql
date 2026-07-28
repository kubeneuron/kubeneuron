-- 0011 — server-side action protocol hardening for failover.
--
-- attempts counts how many leases the controller has issued for one action:
-- replays after crash/failover attach to the same deterministic action and
-- become visible here instead of silently dispatching twice.
-- executor_boot_id records the node boot that claimed the action, so a
-- result posted after an unnoticed reboot is rejected as evidence.
-- Cancellation is a new terminal state reachable only from 'pending':
-- delivered work can finish or expire, never be silently revoked.

ALTER TABLE actions ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE actions ADD COLUMN executor_boot_id TEXT NOT NULL DEFAULT '';
