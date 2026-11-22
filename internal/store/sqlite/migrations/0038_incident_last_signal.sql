-- SPDX-License-Identifier: Apache-2.0
-- When a signal last joined the incident: the verification quiet window is
-- anchored on it, not on updated_at, which other writes move too. Until now
-- updated_at moved in VERIFYING only on a signal, so it is the starting value.
ALTER TABLE incidents ADD COLUMN last_signal_at TEXT NOT NULL DEFAULT '';
UPDATE incidents SET last_signal_at = updated_at;
