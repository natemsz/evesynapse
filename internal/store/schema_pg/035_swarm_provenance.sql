-- Schema step 035: where an applied swarm entry came from.
--
-- Verification is sampled, so most of a trusted signer's entries
-- reach the real tables on signature and shape alone. Until now
-- nothing recorded which rows those were: excluding a signer
-- stopped new entries and left everything it had already
-- supplied in place.
--
-- swarm_applied keeps one row per applied key: the instance the
-- entry was pulled from, the instance whose key signed it, the
-- fetch time that signature covers, when it was applied here,
-- whether ESI confirmed it first, and the stamp written into
-- the live row (row_stamp) so a later rollback can tell the
-- swarm's copy from one this instance has since fetched itself.
-- Market history also keeps the day range it supplied. When a
-- signer is excluded, the worker walks its rows here and takes
-- back what is still the swarm's copy.
--
-- swarm_staging gains source for the same reason: the serving
-- peer has to survive from staging to apply.

ALTER TABLE swarm_staging ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS swarm_applied (
    kind        TEXT NOT NULL,
    key         TEXT NOT NULL,
    source      TEXT NOT NULL,
    signer      TEXT NOT NULL,
    fetched_at  timestamptz NOT NULL,
    applied_at  timestamptz NOT NULL,
    verified    BOOLEAN NOT NULL DEFAULT FALSE,
    row_stamp   timestamptz,
    first_date  TEXT NOT NULL DEFAULT '',
    last_date   TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (kind, key)
);

CREATE INDEX IF NOT EXISTS swarm_applied_signer ON swarm_applied (signer, applied_at);
