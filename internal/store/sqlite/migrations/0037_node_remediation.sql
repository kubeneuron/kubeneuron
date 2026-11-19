-- SPDX-License-Identifier: Apache-2.0
-- Each installation uses its own workflow store. A recreated Node has a new UID.
CREATE TABLE node_remediation_owners (
    node TEXT NOT NULL,
    node_uid TEXT NOT NULL,
    incident_id TEXT NOT NULL UNIQUE REFERENCES incidents(id) ON DELETE CASCADE,
    PRIMARY KEY (node, node_uid)
);

-- Keep the link to the incident after it releases ownership, so the follower
-- can verify the repair against its own signal across controller restarts.
CREATE TABLE node_remediation_followers (
    incident_id TEXT PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
    owner_id TEXT NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    CHECK (incident_id <> owner_id)
);
CREATE INDEX node_remediation_followers_owner ON node_remediation_followers(owner_id);

ALTER TABLE incidents ADD COLUMN repair_node_uid TEXT NOT NULL DEFAULT '';

-- Serialize admission of GPU and whole-node work on a node incarnation.
CREATE TABLE repair_step_guards (
    node TEXT NOT NULL,
    node_uid TEXT NOT NULL,
    PRIMARY KEY (node, node_uid)
);
CREATE TABLE repair_step_claims (
    incident_id TEXT PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
    node TEXT NOT NULL,
    node_uid TEXT NOT NULL,
    action_id TEXT NOT NULL,
    device_step INTEGER NOT NULL CHECK (device_step IN (0, 1)),
    FOREIGN KEY (node, node_uid) REFERENCES repair_step_guards(node, node_uid)
);
CREATE INDEX repair_step_claims_node ON repair_step_claims(node, node_uid, device_step);
