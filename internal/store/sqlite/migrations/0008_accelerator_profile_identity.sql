-- 0008 — bind accelerator reports to an exact runtime-profile revision.
--
-- A digest alone is insufficient for a Kubernetes-native acknowledgement:
-- deleting and recreating a profile, or changing its non-digest policy, can
-- retain the same image/runtime asset digest. The UID plus metadata.generation
-- makes an old report visibly stale to the controller capability gate.

ALTER TABLE accelerator_reports ADD COLUMN profile_uid TEXT NOT NULL DEFAULT '';
ALTER TABLE accelerator_reports ADD COLUMN profile_generation INTEGER NOT NULL DEFAULT 0;
