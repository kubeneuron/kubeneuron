-- 0002 — server-side action protocol hardening (see the SQLite 0011 notes).

ALTER TABLE actions ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE actions ADD COLUMN executor_boot_id TEXT NOT NULL DEFAULT '';
