-- SPDX-License-Identifier: Apache-2.0
-- Legacy inventory remains observation-only until the agent reports its boot.
ALTER TABLE accelerator_reports ADD COLUMN boot_id TEXT NOT NULL DEFAULT '';
