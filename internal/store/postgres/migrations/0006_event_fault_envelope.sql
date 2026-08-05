-- 0006 — persist the vendor-neutral fault envelope and PCI address through the
-- durable event outbox (see the SQLite 0015 notes).
--
-- The controller classifies an event AFTER reading it back from the outbox, so
-- dropping AgentEvent.Fault / AgentEvent.PCIAddr on archival silently killed the
-- nvidia-smi/DCGM fallback detection source: an XID=0 + Fault{nvidia, ecc-dbe}
-- event lost its Fault, ClassifyXID(0) failed, and no incident opened.
--
-- fault_json stores the JSON-marshalled *FaultSignal; the empty-string default
-- means "no fault" and keeps legacy rows scanning back to a nil Fault.
ALTER TABLE events ADD COLUMN IF NOT EXISTS fault_json TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN IF NOT EXISTS pci_addr   TEXT NOT NULL DEFAULT '';
