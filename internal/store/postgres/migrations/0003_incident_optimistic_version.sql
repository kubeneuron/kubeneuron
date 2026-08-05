-- Optimistic-concurrency guard for incidents. UpdateIncident bumps this
-- monotonic counter and matches on its prior value, so a writer working from a
-- stale snapshot (a step transition racing a concurrent signal ingest) can no
-- longer clobber a newer row. A 0-row update is reported as a conflict the
-- caller retries, instead of a silent last-write-wins overwrite.
ALTER TABLE incidents ADD COLUMN version INTEGER NOT NULL DEFAULT 0;
