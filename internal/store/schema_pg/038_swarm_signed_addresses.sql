-- Schema step 038: a member's address carries its owner's
-- signature, and seeds stop being a special identity.
--
-- url_sig is the member's own Ed25519 signature over
-- "member\n" + instance_id + "\n" + base_url + "\n" + issued_at.
-- It travels with the member tuple and is stored and re-served
-- verbatim, so an address is always its owner's word, whoever
-- relayed it. url_issued_at is the owner's clock when it signed
-- (unix seconds): only a tuple issued later than the one held
-- replaces it. An empty url_sig means this instance knows the
-- peer only by an address it dialed itself; such a row is never
-- relayed.
--
-- A seed is now a member like any other, with its own instance
-- ID and key, so nothing can single it out by ID. Whether a peer
-- serves data is learned instead: last_served records the last
-- pull that staged anything from it, and only peers that have
-- served lately are in the data rotation. last_pulled orders
-- that rotation and, while NULL, grants a peer its one probe.

ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS url_sig TEXT NOT NULL DEFAULT '';
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS url_issued_at BIGINT NOT NULL DEFAULT 0;
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS last_pulled timestamptz;
ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS last_served timestamptz;

-- Every seed worker used to sign as the one instance ID "seed".
-- No real member ever held that ID, and after this step no seed
-- does: the row is removed here so nobody has to clear it by
-- hand. The seed returns under its own ID at the next gossip.
DELETE FROM swarm_staging WHERE signer = 'seed';
DELETE FROM swarm_applied WHERE signer = 'seed';
DELETE FROM swarm_peers WHERE instance_id = 'seed';
