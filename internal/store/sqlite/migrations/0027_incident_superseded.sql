-- SPDX-License-Identifier: Apache-2.0

-- Target changes are terminal without claiming recovery.
DROP INDEX IF EXISTS idx_incidents_open_attributed;
DROP INDEX IF EXISTS idx_incidents_open_unattributed;
CREATE UNIQUE INDEX idx_incidents_open_attributed ON incidents (node, gpu_uuid, class)
 WHERE gpu_uuid <> '' AND state NOT IN ('RESOLVED', 'EXPIRED', 'SUPERSEDED');
CREATE UNIQUE INDEX idx_incidents_open_unattributed ON incidents (node, pci_addr, class)
 WHERE gpu_uuid = '' AND state NOT IN ('RESOLVED', 'EXPIRED', 'SUPERSEDED');
