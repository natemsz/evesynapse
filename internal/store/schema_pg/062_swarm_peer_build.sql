-- Schema step 062: the build each swarm peer last claimed
-- for itself, shown on the Admin page beside its role.
--
-- build is self-reported display metadata only. Nothing in
-- the trust model reads it: it never approves, excludes,
-- weights a spot check, or moves a counter, and a peer
-- changing its claim (an upgrade, a downgrade, a different
-- build entirely) changes nothing but this label. Peers that
-- predate the claim carry the empty string, shown as unknown.

ALTER TABLE swarm_peers ADD COLUMN IF NOT EXISTS build TEXT NOT NULL DEFAULT '';
