-- SPDX-License-Identifier: Apache-2.0
ALTER TABLE training_progress ADD COLUMN ready_at TEXT;
ALTER TABLE training_progress ADD COLUMN confirmed_at TEXT;
