-- 0009 — bind runtime evidence to immutable Kubernetes Node identity.
--
-- Node names can be reused after delete/recreate. A current Pod-bound agent
-- token proves the Node UID, and accelerator reports retain that server-owned
-- value so the capability gate cannot reuse a report from the old Node object.

ALTER TABLE nodes ADD COLUMN node_uid TEXT NOT NULL DEFAULT '';
ALTER TABLE accelerator_reports ADD COLUMN node_uid TEXT NOT NULL DEFAULT '';
