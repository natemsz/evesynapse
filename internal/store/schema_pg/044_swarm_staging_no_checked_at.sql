-- Schema step 044: swarm_staging loses checked_at.
--
-- A staged entry leaves the table when it is checked (applied,
-- discarded or dropped), so checked_at was never set: every row read
-- NULL, and the query that would have set it was never called. The
-- column and the partial index on it go; the spot-check pass reads
-- the table in its own order (untried first, oldest first), which the
-- new index serves.

DROP INDEX IF EXISTS swarm_staging_unchecked;
ALTER TABLE swarm_staging DROP COLUMN IF EXISTS checked_at;
CREATE INDEX IF NOT EXISTS swarm_staging_order ON swarm_staging (deferrals, received_at);
