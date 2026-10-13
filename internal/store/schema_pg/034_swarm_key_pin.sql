-- Schema step 034: a refused key change leaves a mark.
--
-- The first key seen for an instance ID is pinned: a later
-- sighting of that ID under another key is refused, never
-- written. key_conflict_at records when that last happened, so
-- the Admin page can show the operator which instance to
-- re-pair. Removing the peer (the only way to accept a new key)
-- drops the row and the mark with it.

ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS key_conflict_at timestamptz;
