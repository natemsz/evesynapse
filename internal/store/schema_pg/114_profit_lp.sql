-- Schema step 114: LP earnings per store.
--
-- ESI serves each character's LP balance per corporation (cached an
-- hour) and no LP store purchase log, and nothing for corporations'
-- LP. So every purchase is inferred, from a balance drop bracketed
-- between two samples:
--   * the drop is solved against the corporation's stored offers
--     (lp_offers): offer and quantity pairs whose LP adds up to it,
--     and whose ISK adds up to the journal's lp_store payments in the
--     bracket when any are there (their absence voids nothing);
--   * on a drop the character's assets are asked for again once their
--     cache runs out, and the diff against the assets held at the drop
--     (by quantity, since purchases merge into stacks) corroborates
--     the product.
-- Tiers say what a purchase may claim: 'corroborated' and 'inferred'
-- (one offer solution; the product seen, or not) make stock lots and
-- count in realized ISK per LP; 'ambiguous' (several solutions) and
-- 'unexplained' (none) count only as LP and ISK spent at the store
-- until the player picks ('user_confirmed').

CREATE TABLE IF NOT EXISTS lp_balance_samples (
    character_id   BIGINT NOT NULL,
    corporation_id BIGINT NOT NULL,
    sampled_at     TIMESTAMPTZ NOT NULL,
    points         BIGINT NOT NULL,
    PRIMARY KEY (character_id, corporation_id, sampled_at)
);
CREATE INDEX IF NOT EXISTS lp_balance_samples_day_idx ON lp_balance_samples (character_id, sampled_at);

CREATE TABLE IF NOT EXISTS lp_purchases (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    character_id    BIGINT NOT NULL,
    corporation_id  BIGINT NOT NULL,
    from_at         TIMESTAMPTZ NOT NULL, -- the bracket: after the sample before the drop
    to_at           TIMESTAMPTZ NOT NULL, -- and no later than the sample that showed it
    lp_spent        BIGINT NOT NULL,
    isk_paid        DOUBLE PRECISION NOT NULL DEFAULT 0, -- the offers' ISK, or the journal's when no offer fits
    tier            TEXT NOT NULL,        -- corroborated | inferred | ambiguous | unexplained | user_confirmed
    offer_id        BIGINT NOT NULL DEFAULT 0,
    quantity        BIGINT NOT NULL DEFAULT 0,  -- offers bought
    product_type_id BIGINT NOT NULL DEFAULT 0,
    units           BIGINT NOT NULL DEFAULT 0,  -- product units acquired
    req_value       DOUBLE PRECISION NOT NULL DEFAULT 0, -- required items at the house price that day
    candidates      TEXT NOT NULL DEFAULT '',   -- JSON [{offer_id, quantity}] when ambiguous
    journal_isk     DOUBLE PRECISION NOT NULL DEFAULT 0, -- lp_store payments seen in the bracket
    -- The timed asset check: product quantity held at the drop, from
    -- assets fetched when; checked once a later fetch lands.
    assets_before   BIGINT NOT NULL DEFAULT 0,
    assets_as_of    TIMESTAMPTZ,
    checked         BOOLEAN NOT NULL DEFAULT false,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (character_id, corporation_id, to_at)
);
CREATE INDEX IF NOT EXISTS lp_purchases_char_idx ON lp_purchases (character_id, corporation_id, to_at);

-- What an LP purchase's units sold for: a sale's takes from its lot.
CREATE TABLE IF NOT EXISTS lp_sale_links (
    pool_kind   TEXT NOT NULL,
    pool_id     BIGINT NOT NULL,
    purchase_id BIGINT NOT NULL,
    type_id     BIGINT NOT NULL,
    owner_id    BIGINT NOT NULL,
    transaction_id BIGINT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    units       BIGINT NOT NULL,
    revenue     DOUBLE PRECISION NOT NULL,
    cost        DOUBLE PRECISION NOT NULL, -- the lot's ISK at LP = 0
    PRIMARY KEY (pool_kind, pool_id, purchase_id, transaction_id)
);
CREATE INDEX IF NOT EXISTS lp_sale_links_type_idx ON lp_sale_links (pool_kind, pool_id, type_id, at);

-- Per pool and store: the totals the LP section shows.
CREATE TABLE IF NOT EXISTS lp_store_summary (
    pool_kind        TEXT NOT NULL,
    pool_id          BIGINT NOT NULL,
    corporation_id   BIGINT NOT NULL,
    lp_earned        BIGINT NOT NULL DEFAULT 0,
    lp_corroborated  BIGINT NOT NULL DEFAULT 0,
    lp_inferred      BIGINT NOT NULL DEFAULT 0,
    lp_ambiguous     BIGINT NOT NULL DEFAULT 0,
    lp_unexplained   BIGINT NOT NULL DEFAULT 0,
    isk_paid         DOUBLE PRECISION NOT NULL DEFAULT 0,
    units_acquired   BIGINT NOT NULL DEFAULT 0,
    units_sold       BIGINT NOT NULL DEFAULT 0,
    revenue          DOUBLE PRECISION NOT NULL DEFAULT 0,
    tax              DOUBLE PRECISION NOT NULL DEFAULT 0,
    sold_cost        DOUBLE PRECISION NOT NULL DEFAULT 0, -- ISK and required items of the units sold
    lp_sold          DOUBLE PRECISION NOT NULL DEFAULT 0, -- LP behind the units sold
    unsold_value     DOUBLE PRECISION NOT NULL DEFAULT 0, -- unsold units at today's stat pack
    unsold_priced    BOOLEAN NOT NULL DEFAULT true,
    folded_at        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pool_kind, pool_id, corporation_id)
);

-- The daily rows keep LP apart from ISK: income and costs are the
-- ISK at an LP cost of 0, lp_delta the net LP earned less spent that
-- day, so profit at any LP rate is one multiplication.
ALTER TABLE profit_daily ADD COLUMN IF NOT EXISTS lp_delta BIGINT NOT NULL DEFAULT 0;

-- The player's own ISK per LP for the sensitivity line (0: unset).
ALTER TABLE user_profit_prefs ADD COLUMN IF NOT EXISTS lp_isk_value DOUBLE PRECISION NOT NULL DEFAULT 0;

-- LP store payments were filed as running costs; they are the cost of
-- LP lots now, counted when the units sell.
UPDATE ledger_journal SET activity = 'lp_store' WHERE ref_type = 'lp_store';
UPDATE ledger_pool_state SET members = '';
