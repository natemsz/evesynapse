-- Schema step 102: the LP store browser.
--
-- The worker mirrors every NPC corporation's loyalty store from
-- public ESI (lp_store_corps, lp_offers, lp_offer_requirements),
-- and values each offer per swept hub region into lp_offer_values
-- from the stat pack (market_region_stats): when a region's sweep
-- completes, and when a store's offers change. The page reads
-- lp_offer_values only, ranked in SQL with a LIMIT.
--
-- An offer is priced only when its product and every required
-- item have a typical price in that region; otherwise it is
-- stored unpriced (an honest gap) with the reason. Blueprint copy
-- products are never priced from their original's market.

CREATE TABLE IF NOT EXISTS lp_store_corps (
    corporation_id BIGINT PRIMARY KEY,
    name           TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'pending',
    offer_count    BIGINT NOT NULL DEFAULT 0,
    fetched_at     TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS lp_offers (
    corporation_id BIGINT NOT NULL,
    offer_id       BIGINT NOT NULL,
    type_id        BIGINT NOT NULL,
    quantity       BIGINT NOT NULL DEFAULT 1,
    lp_cost        BIGINT NOT NULL DEFAULT 0,
    isk_cost       DOUBLE PRECISION NOT NULL DEFAULT 0,
    ak_cost        BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (corporation_id, offer_id)
);

CREATE TABLE IF NOT EXISTS lp_offer_requirements (
    corporation_id BIGINT NOT NULL,
    offer_id       BIGINT NOT NULL,
    type_id        BIGINT NOT NULL,
    quantity       BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (corporation_id, offer_id, type_id)
);

CREATE TABLE IF NOT EXISTS lp_offer_values (
    region_id      BIGINT NOT NULL,
    corporation_id BIGINT NOT NULL,
    offer_id       BIGINT NOT NULL,
    type_id        BIGINT NOT NULL,
    quantity       BIGINT NOT NULL,
    lp_cost        BIGINT NOT NULL,
    isk_cost       DOUBLE PRECISION NOT NULL,
    required_cost  DOUBLE PRECISION NOT NULL DEFAULT 0,
    unit_sell      DOUBLE PRECISION NOT NULL DEFAULT 0,
    unit_buy       DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- ISK per LP selling into sell orders at the typical sell, and
    -- selling at once into buy orders at the typical buy; NULL when
    -- unpriced or the side has no orders.
    isk_per_lp_sell DOUBLE PRECISION,
    isk_per_lp_buy  DOUBLE PRECISION,
    unpriced_reason TEXT NOT NULL DEFAULT '',
    daily_volume    DOUBLE PRECISION NOT NULL DEFAULT 0,
    sell_chg_7d     DOUBLE PRECISION,
    volatility_30d  DOUBLE PRECISION,
    prices_at       TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region_id, corporation_id, offer_id)
);

CREATE INDEX IF NOT EXISTS lp_offer_values_rank_idx
    ON lp_offer_values (region_id, isk_per_lp_sell DESC NULLS LAST);
