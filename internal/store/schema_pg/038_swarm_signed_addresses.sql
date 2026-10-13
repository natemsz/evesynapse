-- Schema step 038: a member's address carries its owner's
-- signature, and seeds stop being a special identity.
--
-- url_sig is the member's own Ed25519 signature over
-- "member\n" + instance_id + "\n" + base_url. It travels with
-- the member tuple and is stored and re-served verbatim, so an
-- address is always its owner's word, whoever relayed it. Empty
-- means this instance knows the peer only by an address it
-- dialed itself; such a row is never relayed.
--
-- A seed is now a member like any other, with its own instance
-- ID and key, so nothing can single it out by ID. last_pulled
-- lets pulls take peers in turn (a directory that serves nothing
-- costs a slot now and then, never every cycle), and last_served
-- records the last pull that staged anything.

ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS url_sig TEXT NOT NULL DEFAULT '';
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS last_pulled timestamptz;
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS last_served timestamptz;
