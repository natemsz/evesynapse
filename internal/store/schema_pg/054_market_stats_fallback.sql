-- Schema step 054: the Fuzzwork fallback stats.
--
-- EveSynapse prices from its own ESI sweeps first, but the
-- sweeps cover the five hub regions; everywhere else a type
-- showed no typical price at all, or a CCP guide number the
-- house no longer trusts as a price. Fuzzwork's community
-- aggregate scrape covers every region with live orders, so
-- the worker mirrors it here -- in its own table, never in
-- the sweep tables, with the provenance on every row: the
-- source, the orderset the row came from, and when that
-- orderset was current. Only the two statistics that joke
-- orders cannot move are kept (the median and the
-- five-percent band average); the weighted average and the
-- extremes are never stored, so they can never be quoted.
-- A row whose side has no orders is absent data, not a
-- price of zero.

CREATE TABLE IF NOT EXISTS market_stats_fallback (
    region_id    BIGINT NOT NULL,
    type_id      BIGINT NOT NULL,
    median_sell  DOUBLE PRECISION NOT NULL DEFAULT 0,
    fivepct_sell DOUBLE PRECISION NOT NULL DEFAULT 0,
    median_buy   DOUBLE PRECISION NOT NULL DEFAULT 0,
    fivepct_buy  DOUBLE PRECISION NOT NULL DEFAULT 0,
    sell_orders  BIGINT NOT NULL DEFAULT 0,
    buy_orders   BIGINT NOT NULL DEFAULT 0,
    sell_volume  DOUBLE PRECISION NOT NULL DEFAULT 0,
    buy_volume   DOUBLE PRECISION NOT NULL DEFAULT 0,
    source       TEXT NOT NULL DEFAULT 'fuzzwork',
    orderset_id  BIGINT NOT NULL,
    source_asof  TIMESTAMPTZ NOT NULL,
    fetched_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region_id, type_id)
);

-- The mirror's one bookkeeping row (id = 1): which orderset
-- the table currently holds, so an hourly poll that finds
-- the same orderset downloads nothing.
CREATE TABLE IF NOT EXISTS market_stats_fallback_meta (
    id          INTEGER PRIMARY KEY,
    orderset_id BIGINT NOT NULL DEFAULT 0,
    fetched_at  TIMESTAMPTZ,
    row_count   BIGINT NOT NULL DEFAULT 0
);
