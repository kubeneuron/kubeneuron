-- See sqlite 0019: the reset preflight needs the incident's accelerator
-- vendor to tell an impossible reset from one whose evidence has not arrived
-- yet. Empty means unknown and imposes no constraint.
ALTER TABLE incidents ADD COLUMN IF NOT EXISTS vendor TEXT NOT NULL DEFAULT '';
