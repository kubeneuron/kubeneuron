-- 0015 — persist the vendor-neutral fault envelope and PCI address through the
-- durable event outbox.
--
-- Before this, the events table stored only (event_id, node, gpu_index,
-- gpu_uuid, xid, raw, timestamp), so the R6 fault envelope (AgentEvent.Fault)
-- and AgentEvent.PCIAddr were dropped on the archive round trip. The controller
-- classifies an event AFTER it reads the row back from the outbox, so a
-- gpuhealth/nvidia-smi fallback event carrying XID=0 + Fault{nvidia, ecc-dbe}
-- lost its Fault, ClassifyXID(0) failed, and the event was durably acknowledged
-- as non-actionable — a double-bit ECC error seen only by the nvidia-smi/DCGM
-- fallback was silently lost.
--
-- fault_json stores the JSON-marshalled *FaultSignal; the empty-string default
-- means "no fault" and keeps legacy rows (which only ever carried an XID)
-- scanning back to a nil Fault rather than a fabricated one. pci_addr carries
-- the GPU PCI address the kmsg/DCGM source knew.
ALTER TABLE events ADD COLUMN fault_json TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN pci_addr   TEXT NOT NULL DEFAULT '';
