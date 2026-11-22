-- SPDX-License-Identifier: Apache-2.0
-- Where a node row comes from (the agent's registration, or the Kubernetes
-- Node object of a node without an agent) and, for the latter, the
-- accelerator devices the Node advertises.
ALTER TABLE nodes ADD COLUMN inventory_source TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN devices TEXT NOT NULL DEFAULT '[]';
UPDATE nodes SET inventory_source = 'agent' WHERE agent_last_seen IS NOT NULL;
