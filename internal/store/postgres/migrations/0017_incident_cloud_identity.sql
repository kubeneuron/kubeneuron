-- SPDX-License-Identifier: Apache-2.0
-- Legacy incidents have no trustworthy cloud identity snapshot.
ALTER TABLE incidents ADD COLUMN cloud_identity TEXT NOT NULL DEFAULT 'null';
