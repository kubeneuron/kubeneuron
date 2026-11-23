-- SPDX-License-Identifier: Apache-2.0
ALTER TABLE nodes ADD COLUMN deleted_at TEXT;
CREATE INDEX nodes_deleted_at_idx ON nodes (deleted_at);
