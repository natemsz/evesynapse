-- Schema step 037: how often a staged entry has been deferred.
--
-- A spot check that cannot decide (no character with the scope,
-- no span shared with ESI's history, a transient error) leaves
-- the entry staged for the next pass. Nothing bounded that: an
-- undecidable entry took a check slot every cycle until the
-- seven-day age cap, and was then discarded as a failed check
-- against its signer though nothing had ever been compared.
--
-- deferrals counts those passes, so an entry that keeps
-- deferring is dropped after a few, with no mark on anyone, and
-- waits behind entries that have not been tried yet.

ALTER TABLE swarm_staging ADD COLUMN IF NOT EXISTS deferrals INT NOT NULL DEFAULT 0;
