-- Schema step 121: the stat pack's 30-day price line.
--
-- Every listed price can carry a hover card with its recent
-- history. The card's figures are derived once per sweep, beside
-- the stat pack's trend figures and in the same transaction, from
-- the daily rows just written:
--
--   * sell_spark_30d: the typical sell over the last 30 days as
--     polyline points in the site's sparkline box (100 by 28, 2
--     units of padding), oldest first; empty under two days.
--   * sell_low_30d, sell_high_30d, sell_mean_30d: the same days'
--     lowest, highest and average typical sell (0 when empty).
--
-- Pages read these stored figures; none of them re-derive a line.

ALTER TABLE market_region_stats
    ADD COLUMN IF NOT EXISTS sell_spark_30d TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sell_low_30d   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_high_30d  DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_mean_30d  DOUBLE PRECISION NOT NULL DEFAULT 0;
