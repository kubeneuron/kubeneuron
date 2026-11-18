-- SPDX-License-Identifier: Apache-2.0
-- The accelerator devices an incident covered when it opened, from the
-- node's inventory at that moment, so the recovery report counts each
-- resource in its own unit without reading today's inventory.
ALTER TABLE incidents ADD COLUMN accelerators TEXT NOT NULL DEFAULT 'null';
