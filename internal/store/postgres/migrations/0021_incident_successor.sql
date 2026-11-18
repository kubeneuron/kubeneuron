-- SPDX-License-Identifier: Apache-2.0
-- The add-first successor of an incident's node, and whether its operation is
-- still open, so abandoned successors can be found without decoding every row.
ALTER TABLE incidents ADD COLUMN successor TEXT NOT NULL DEFAULT 'null';
ALTER TABLE incidents ADD COLUMN successor_open INTEGER NOT NULL DEFAULT 0;
