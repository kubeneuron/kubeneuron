-- An incident about a device must be able to name that device even when the
-- device is too broken to name itself.
--
-- A kernel fault that knocks a GPU off the bus prints the PCI address and
-- nothing else: nvidia-smi no longer lists the device, so the agent cannot
-- resolve a UUID. Until now that produced an incident whose only identity was
-- (node, '', class), and that cost operators twice.
--
-- First, two DIFFERENT unattributed GPUs on one node with the same problem
-- class collided on that key. The unique index below admitted only one of
-- them, so the second GPU's fault was folded into the first GPU's incident and
-- was never remediated or reported as its own failure.
--
-- Second, when the vendor tool resolved the SAME PCI address to a real UUID
-- seconds later, there was no column to match it against, so the precise
-- signal could only be attached to a vague incident that stayed unattributed.
-- The ladder then cordoned and drained the node, reached the reset rung, and
-- refused it permanently with "reset target unattributed" — for a device whose
-- exact identity had been available the whole time.
--
-- pci_addr stores the NORMALIZED address (types.NormalizePCIAddress:
-- lowercase, no function suffix, four-digit domain), because the sources spell
-- it differently and a raw value would not compare equal to itself.
ALTER TABLE incidents ADD COLUMN pci_addr TEXT NOT NULL DEFAULT '';

-- The open-incident uniqueness rule splits in two, because the two halves of
-- the key mean different things.
--
-- An ATTRIBUTED incident is still unique per (node, gpu_uuid, class): that is
-- exactly the guarantee the old index gave, unchanged, and it is what makes a
-- promotion safe. Promoting an unattributed row onto a UUID that already has
-- an open incident of the same class is refused by the DATABASE rather than by
-- a check that raced, and the promotion fails loudly instead of producing two
-- open incidents for one device.
--
-- An UNATTRIBUTED incident is unique per (node, pci_addr, class), so two GPUs
-- that fall off the same node's bus keep two incidents. Rows written before
-- this migration have pci_addr '', which reproduces the old (node, '', class)
-- behaviour for them exactly, so upgrading cannot violate either index.
DROP INDEX IF EXISTS idx_incidents_open;

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open_attributed
    ON incidents (node, gpu_uuid, class)
    WHERE gpu_uuid <> '' AND state NOT IN ('RESOLVED', 'EXPIRED');

CREATE UNIQUE INDEX IF NOT EXISTS idx_incidents_open_unattributed
    ON incidents (node, pci_addr, class)
    WHERE gpu_uuid = '' AND state NOT IN ('RESOLVED', 'EXPIRED');
