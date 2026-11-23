-- SPDX-License-Identifier: Apache-2.0
-- Older controllers must refuse this schema before sweeping versioned delete intents.
CREATE INDEX incidents_open_remove_first_idx ON incidents (id) WHERE remove_first_open=1;
