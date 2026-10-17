-- Schema step 052: true-value evidence tiers.
--
-- Step 051's estimated sell rested on one kind of evidence:
-- live public-contract listings (asks). Two more first-party
-- tiers join it, and the stored row now says which tier its
-- estimated sell rests on:
--
--   sell_source  '' while the row is anchored to build cost;
--                otherwise the tier whose samples produced the
--                estimated sell: 'fulfilled' (contracts of
--                linked characters that ESI reports finished
--                -- certain sales), 'vanished' (public
--                listings the crawl watched disappear before
--                expiry -- likely sold, never certain: an
--                issuer can also delete a listing), or
--                'listing' (live public asks, as before).
--
-- The crawl's longitudinal memory lives in its own table:
-- one row per public contract the sampler accepted, kept so
-- a later crawl can tell whether the listing is still there.
-- A row whose listing vanishes well before expiry gains a
-- vanished_at and becomes probable-sale evidence; a row that
-- vanishes at expiry is deleted (expiry explains it). Rows
-- are evidence bookkeeping only -- pages never read them.

ALTER TABLE true_values ADD COLUMN IF NOT EXISTS sell_source TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS true_value_contract_observations (
    contract_id BIGINT PRIMARY KEY,
    region_id BIGINT NOT NULL,
    type_id BIGINT NOT NULL,
    shape TEXT NOT NULL DEFAULT 'single',
    listed_price DOUBLE PRECISION NOT NULL DEFAULT 0,
    sample_price DOUBLE PRECISION NOT NULL DEFAULT 0,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    vanished_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS true_value_contract_observations_type_idx
    ON true_value_contract_observations (type_id, vanished_at);
