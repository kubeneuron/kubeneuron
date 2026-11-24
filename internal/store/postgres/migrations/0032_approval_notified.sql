-- SPDX-License-Identifier: Apache-2.0
-- Which notification channels accepted the request of the incident's current
-- approval round, as {"epoch":N,"channels":[...]}. Empty until a channel
-- accepts one, so a round waiting at upgrade is requested again once.
ALTER TABLE incidents ADD COLUMN approval_notified TEXT NOT NULL DEFAULT '';
