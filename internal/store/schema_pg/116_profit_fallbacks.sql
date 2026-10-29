-- Schema step 116: profit fallbacks.
--
-- An LP store drop that mixed two offers is solvable when the
-- journal shows the bracket's LP store payment: two offers, two
-- equations (LP and ISK), so each offer's quantity is exact. Such a
-- bracket stores one purchase row per offer.
--
-- A job's output can be tied to one of the player's open sell orders:
-- that character's sales of the item at the order's station, from when
-- the watch was set until the order closed, link to the job.

ALTER TABLE lp_purchases DROP CONSTRAINT IF EXISTS lp_purchases_character_id_corporation_id_to_at_key;
ALTER TABLE lp_purchases DROP CONSTRAINT IF EXISTS lp_purchases_bracket_offer_key;
ALTER TABLE lp_purchases ADD CONSTRAINT lp_purchases_bracket_offer_key UNIQUE (character_id, corporation_id, to_at, offer_id);

CREATE TABLE IF NOT EXISTS job_order_watches (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pool_kind    TEXT NOT NULL,
    pool_id      BIGINT NOT NULL,
    job_id       BIGINT NOT NULL,
    character_id BIGINT NOT NULL,
    order_id     BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (pool_kind, pool_id, character_id, order_id)
);
