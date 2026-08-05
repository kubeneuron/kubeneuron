-- Bind an approval decision to the identity of the step it was made for, not
-- just the incident. A playbook hot-swap or an incident rewind can change the
-- action at the incident's current step index while a decision is pending; the
-- resume path compares these columns against the current step and fails closed
-- (re-parks and re-requests approval) on a mismatch, so a granted approval can
-- never execute an action the human never saw. Empty for pre-existing rows.
ALTER TABLE approvals ADD COLUMN playbook_name TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN step_action TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN step_hash TEXT NOT NULL DEFAULT '';
