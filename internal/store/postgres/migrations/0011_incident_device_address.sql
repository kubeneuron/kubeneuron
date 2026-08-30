-- See sqlite 0020: an incident opened by a kernel fault can name its device
-- only by PCI address, and without that column the incident could neither be
-- told apart from a sibling GPU's nor promoted onto the UUID a later, precise
-- signal carried. The node was cordoned and drained and then parked for a
-- human with "reset target unattributed".
--
-- pci_addr stores the NORMALIZED address (types.NormalizePCIAddress).
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS pci_addr TEXT NOT NULL DEFAULT '';

-- The open-incident uniqueness rule splits in two, exactly as in sqlite 0020.
-- Attributed incidents keep the old (node, gpu_uuid, class) guarantee, which
-- is what makes the database — rather than a check that raced — refuse a
-- promotion onto a UUID that already has an open incident of this class.
-- Unattributed incidents become unique per (node, pci_addr, class) so two GPUs
-- that fell off one node's bus keep two incidents. Pre-migration rows have
-- pci_addr '', which reproduces the old behaviour for them exactly.
DROP INDEX IF EXISTS idx_incidents_open;

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open_attributed
    ON incidents (node, gpu_uuid, class)
    WHERE gpu_uuid <> '' AND state NOT IN ('RESOLVED', 'EXPIRED');

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open_unattributed
    ON incidents (node, pci_addr, class)
    WHERE gpu_uuid = '' AND state NOT IN ('RESOLVED', 'EXPIRED');
