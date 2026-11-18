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
