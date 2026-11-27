-- Schema step 188: precomputed mover metrics on market_region_stats.
--
-- Extends each (region, type) stat pack row with rolling 7-day price,
-- median comparison, trade activity, spread widening and cross-hub
-- confirmation figures written at sweep and history ingest time:
--
--   * sell_avg_7d: rolling 7-day average typical sell from daily sweeps
--     (used to compute sell_chg_7d against the prior 7-day rolling average).
--   * sell_median_7d / sell_median_30d / mover_chg_7v30 / mover_hold_days:
--     7-day vs 30-day median daily sell price move and how many of the
--     last 7 days held the move.
--   * isk_traded_7d / avg_orders_7d / avg_vol_7d / avg_vol_30d:
--     calendar-day averages from regional market_history (actual traded
--     ISK = volume * average, trade count = order_count, and unit volume).
--   * mover_score: ABS(mover_chg_7v30) * LN(1 + isk_traded_7d).
--   * isk_move_7d: ABS(sell_median_7d - sell_median_30d) * avg_vol_7d.
--   * daily_spread_profit: (typical_sell - typical_buy) * LEAST(avg_vol_7d, sell_volume, buy_volume).
--   * spread_pct / spread_widened: current book spread and whether it
--     widened to at least 10% and 2x its baseline.
--   * confirmed_hubs: count of other trade hubs moving at least 5% in
--     the same direction with recorded trade activity.

ALTER TABLE market_region_stats
    ADD COLUMN IF NOT EXISTS sell_avg_7d         DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_median_7d      DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS sell_median_30d     DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS mover_chg_7v30      DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS mover_hold_days     BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS isk_traded_7d       DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS avg_orders_7d       DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS avg_vol_7d          DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS avg_vol_30d         DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS mover_score         DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS isk_move_7d         DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS daily_spread_profit DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS spread_pct          DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS spread_widened      BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS confirmed_hubs      BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS market_region_stats_movers_idx
    ON market_region_stats (region_id, isk_traded_7d DESC, mover_score DESC NULLS LAST);
