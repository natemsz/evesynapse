-- Schema step 101: the market stat pack.
--
-- market_region_stats already holds one row per (region, type),
-- replaced when a region's sweep completes, with a daily copy in
-- market_region_stats_daily. This step widens that row into the
-- canonical stat pack every market surface reads (the scanner,
-- item pages, baskets, the LP store browser, alerts, trade
-- ideas), instead of each one deriving its own figures:
--
--   * the full order-book statistics set per side, distilled from
--     the staged book at sweep completion: average, median
--     (typical_*), both extremes (best_* is the near one), the
--     five-percent edge, standard deviation, volume, order count.
--     Every figure is per order row, like the median. sell_p5 is
--     the price 5 in 100 sell orders are at or under; buy_p5 the
--     price 5 in 100 buy orders are at or over.
--   * trend figures derived from the stored daily rows in the
--     same transaction: percent change of the typical price per
--     side over 1, 7 and 30 days (NULL when no daily row sits in
--     the comparison window), the 30-day volatility of the typical
--     sell (sample standard deviation of daily log returns, in
--     percent; NULL under seven returns), how many daily rows the
--     30-day window holds, and the 30-day average traded volume
--     from market_history.
--
-- The daily table keeps the per-side statistics set too, so
-- history carries the same figures the live row does.
--
-- The Fuzzwork mirror now keeps its full column set (weighted
-- average, extremes, standard deviation). The house price order
-- is unchanged: the fallback still quotes only its median and
-- five-percent figures as a price; the rest is dispersion and
-- depth context, labeled with the mirror's provenance.

ALTER TABLE market_region_stats
    ADD COLUMN IF NOT EXISTS sell_avg             DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_max             DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_p5              DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_stddev          DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_avg              DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_min              DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_p5               DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_stddev           DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_chg_1d          DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS sell_chg_7d          DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS sell_chg_30d         DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS buy_chg_1d           DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS buy_chg_7d           DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS buy_chg_30d          DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS volatility_30d       DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS history_days         BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS avg_daily_volume_30d DOUBLE PRECISION NOT NULL DEFAULT 0;

ALTER TABLE market_region_stats_daily
    ADD COLUMN IF NOT EXISTS sell_avg    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_max    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_p5     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_stddev DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_avg     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_min     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_p5      DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS buy_stddev  DOUBLE PRECISION NOT NULL DEFAULT 0;

-- The trend pass reads one region's recent days at every sweep;
-- the primary key leads with type_id after region_id, so without
-- this a region's whole history would be scanned.
CREATE INDEX IF NOT EXISTS market_region_stats_daily_region_day_idx
    ON market_region_stats_daily (region_id, day);

ALTER TABLE market_stats_fallback
    ADD COLUMN IF NOT EXISTS wavg_sell   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS min_sell    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_sell    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS stddev_sell DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS wavg_buy    DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS min_buy     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_buy     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS stddev_buy  DOUBLE PRECISION NOT NULL DEFAULT 0;
