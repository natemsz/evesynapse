-- Schema step 036: when this instance gossips next.
--
-- The gossip interval is jittered so a swarm does not gossip in
-- lockstep. The jitter used to be drawn again on every check,
-- which made the round fire near the early edge of its window,
-- and the last-gossip time lived in memory, so every restart
-- gossiped at once. next_gossip_at is the one time drawn when a
-- round ends: kept here, it survives a restart. NULL means no
-- round has run yet (or the identity is new): gossip is due.

ALTER TABLE swarm_identity ADD COLUMN IF NOT EXISTS next_gossip_at timestamptz;
