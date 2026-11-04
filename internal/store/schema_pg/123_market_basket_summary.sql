-- Schema step 123: each basket's figures, stored when its values are.
--
-- A basket's daily values are written by the region's sweep (and
-- rebuilt when its items change). The figures every basket surface
-- shows are derived in the same transaction and stored here, so the
-- pages read them instead of deriving them on each load:
--
--   * market_basket_summary, one row per basket: the latest worth at
--     typical sell and buy, how many of its items were priced, the
--     change over 1, 7 and 30 days in percent and in ISK (NULL with
--     no fully priced comparison day), and the 30-day worth line as
--     polyline points for the list's trend column (60 by 16) and for
--     its hover card (100 by 28), with the line's direction and its
--     low, high and mean.
--   * market_basket_item_stats, one row per item: its unit price,
--     its worth, its share of the basket, and its price change and
--     the ISK it moved the basket over 1, 7 and 30 days.

CREATE TABLE IF NOT EXISTS market_basket_summary (
    basket_id    BIGINT PRIMARY KEY REFERENCES market_baskets (id) ON DELETE CASCADE,
    value_sell   DOUBLE PRECISION NOT NULL DEFAULT 0,
    value_buy    DOUBLE PRECISION NOT NULL DEFAULT 0,
    priced_items BIGINT NOT NULL DEFAULT 0,
    item_count   BIGINT NOT NULL DEFAULT 0,
    chg_1d       DOUBLE PRECISION,
    chg_7d       DOUBLE PRECISION,
    chg_30d      DOUBLE PRECISION,
    delta_1d     DOUBLE PRECISION,
    delta_7d     DOUBLE PRECISION,
    delta_30d    DOUBLE PRECISION,
    line_30d     TEXT NOT NULL DEFAULT '',
    line_dir     TEXT NOT NULL DEFAULT '',
    spark_30d    TEXT NOT NULL DEFAULT '',
    low_30d      DOUBLE PRECISION NOT NULL DEFAULT 0,
    high_30d     DOUBLE PRECISION NOT NULL DEFAULT 0,
    mean_30d     DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS market_basket_item_stats (
    basket_id  BIGINT NOT NULL REFERENCES market_baskets (id) ON DELETE CASCADE,
    type_id    BIGINT NOT NULL,
    priced     BOOLEAN NOT NULL,
    unit       DOUBLE PRECISION NOT NULL DEFAULT 0,
    value      DOUBLE PRECISION NOT NULL DEFAULT 0,
    share_pct  DOUBLE PRECISION NOT NULL DEFAULT 0,
    chg_1d     DOUBLE PRECISION,
    chg_7d     DOUBLE PRECISION,
    chg_30d    DOUBLE PRECISION,
    impact_1d  DOUBLE PRECISION,
    impact_7d  DOUBLE PRECISION,
    impact_30d DOUBLE PRECISION,
    PRIMARY KEY (basket_id, type_id)
);
