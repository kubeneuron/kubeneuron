-- SPDX-License-Identifier: Apache-2.0
-- NULL identifies incidents for which worker discovery has not succeeded.
ALTER TABLE incidents ADD COLUMN training_workers TEXT;
