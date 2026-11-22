-- SPDX-License-Identifier: Apache-2.0
-- The planned remove-first replacement of an incident's node, and whether the
-- plan is still open, so a restarted controller finds the pools whose size it
-- must still set back without decoding every row.
ALTER TABLE incidents ADD COLUMN remove_first TEXT NOT NULL DEFAULT 'null';
ALTER TABLE incidents ADD COLUMN remove_first_open INTEGER NOT NULL DEFAULT 0;
