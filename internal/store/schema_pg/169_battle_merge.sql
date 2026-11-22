-- Schema step 169: battle logs ready to merge into fleet reports.
--
-- Several pilots' logs of one battle only add up on a shared clock,
-- so each fight also stores its per-party steps cut on fixed 10-second
-- clock boundaries. aligned_at is the received_at of the read those
-- steps came from: when it differs from the fight's received_at, or is
-- NULL, the fight has no usable aligned steps, and a merged curve must
-- call it approximate or leave it out.
ALTER TABLE battle_fight_series
    ADD COLUMN IF NOT EXISTS aligned    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS aligned_at TIMESTAMPTZ;

-- merge_dirty_seq says a fight changed in a way a fleet report built
-- from it must pick up. Each mark takes a new sequence value, so a
-- builder can clear exactly the mark it read and never a newer one.
-- merge_digest is a fingerprint of the matched mails and parties, so a
-- correlation pass that changes nothing leaves the mark alone.
CREATE SEQUENCE IF NOT EXISTS battle_fights_merge_seq;

ALTER TABLE battle_fights
    ADD COLUMN IF NOT EXISTS merge_digest    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS merge_dirty_seq BIGINT;

CREATE INDEX IF NOT EXISTS battle_fights_merge_dirty
    ON battle_fights (merge_dirty_seq)
    WHERE merge_dirty_seq IS NOT NULL;
