-- 0005 — action delivery leases. A claimed action has an opaque token and a
-- short expiry. This lets an agent result be accepted only from the current
-- claimant and lets work be reclaimed after an agent/controller crash.

ALTER TABLE actions ADD COLUMN lease_token TEXT NOT NULL DEFAULT '';
ALTER TABLE actions ADD COLUMN lease_expires_at_ns INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_actions_node_lease
    ON actions (node, state, lease_expires_at_ns, created_at, id);
