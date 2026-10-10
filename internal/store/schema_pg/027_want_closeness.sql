-- Schema step 027: public records are fetched closest first.
--
-- The queues of public records (pilots, corporations, alliances) are
-- drained by how close a record is to somebody: one on screen, then
-- one that a character's own data names, then one a relationship
-- further out (the corporation of somebody a character traded with).
-- priority already said viewed or not; it now says which of the three,
-- so the existing rows move up one: 0 is the new, furthest ring.
--
-- noted_at is when a record was last asked for. Among records equally
-- close, the one asked for most lately goes first.

ALTER TABLE pilot_records ADD COLUMN IF NOT EXISTS noted_at timestamptz;
ALTER TABLE corporation_records ADD COLUMN IF NOT EXISTS noted_at timestamptz;
ALTER TABLE alliance_records ADD COLUMN IF NOT EXISTS noted_at timestamptz;

UPDATE pilot_records SET priority = priority + 1;
