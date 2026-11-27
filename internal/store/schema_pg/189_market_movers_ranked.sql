-- Schema step 189: precomputed ranked movers table for Trends and Market Movers.
--
-- Stores ranked (region, sort_by, mode, period, rank) rows written at
-- sweep and history ingest time so /market/trends/ and the Market Movers
-- widget read bounded rows in rank order with LIMIT only.
--
-- Also backfills existing market_region_stats mover metrics from stored
-- market_history and populates market_movers_ranked so upgraded databases
-- serve full rankings immediately before the next sweep completes.

CREATE TABLE IF NOT EXISTS market_movers_ranked (
    region_id     BIGINT NOT NULL,
    sort_by       TEXT NOT NULL,
    mode          TEXT NOT NULL,
    period        INTEGER NOT NULL,
    rank          INTEGER NOT NULL,
    type_id       BIGINT NOT NULL,
    score         DOUBLE PRECISION NOT NULL,
    isk_traded_7d DOUBLE PRECISION NOT NULL DEFAULT 0,
    avg_orders_7d DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region_id, sort_by, mode, period, rank)
);

WITH hist_ref AS (
    SELECT mh.region_id, mh.type_id,
           CASE WHEN MAX(mh.date)::date >= (now() AT TIME ZONE 'UTC')::date - 2
                 AND MAX(mh.date)::date <= (now() AT TIME ZONE 'UTC')::date
                THEN MAX(mh.date)::date
                ELSE (now() AT TIME ZONE 'UTC')::date - 1
           END AS ref_date
    FROM market_history mh
    GROUP BY mh.region_id, mh.type_id
),
hist AS (
    SELECT mh.region_id, mh.type_id,
           COALESCE(SUM(mh.volume::double precision * mh.average)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS isk_traded_7d,
           COALESCE(SUM(mh.order_count::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_orders_7d,
           COALESCE(SUM(mh.volume::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_vol_7d,
           COALESCE(SUM(mh.volume::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 30.0 AS avg_vol_30d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY mh.average)
               FILTER (WHERE mh.average > 0 AND mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_7d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY mh.average)
               FILTER (WHERE mh.average > 0 AND mh.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_30d
    FROM market_history mh
    JOIN hist_ref hr ON hr.region_id = mh.region_id AND hr.type_id = mh.type_id
    WHERE mh.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD')
    GROUP BY mh.region_id, mh.type_id
),
hold AS (
    SELECT mh.region_id, mh.type_id,
           COUNT(*) FILTER (WHERE
               (h.sell_median_7d > h.sell_median_30d
                AND mh.average >= h.sell_median_30d + 0.5 * (h.sell_median_7d - h.sell_median_30d))
            OR (h.sell_median_7d < h.sell_median_30d
                AND mh.average <= h.sell_median_30d + 0.5 * (h.sell_median_7d - h.sell_median_30d))
           )::bigint AS hold_days
    FROM market_history mh
    JOIN hist_ref hr ON hr.region_id = mh.region_id AND hr.type_id = mh.type_id
    JOIN hist h ON h.region_id = mh.region_id AND h.type_id = mh.type_id
    WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD')
      AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')
      AND mh.average > 0
      AND h.sell_median_7d > 0
      AND h.sell_median_30d > 0
    GROUP BY mh.region_id, mh.type_id
)
UPDATE market_region_stats s SET
    sell_median_7d      = COALESCE(h.sell_median_7d, 0),
    sell_median_30d     = COALESCE(h.sell_median_30d, 0),
    mover_chg_7v30      = CASE WHEN COALESCE(h.sell_median_7d, 0) > 0 AND COALESCE(h.sell_median_30d, 0) > 0
                               THEN (h.sell_median_7d - h.sell_median_30d) / h.sell_median_30d * 100.0 END,
    mover_hold_days     = COALESCE(hd.hold_days, 0),
    isk_traded_7d       = COALESCE(h.isk_traded_7d, 0),
    avg_orders_7d       = COALESCE(h.avg_orders_7d, 0),
    avg_vol_7d          = COALESCE(h.avg_vol_7d, 0),
    avg_vol_30d         = COALESCE(h.avg_vol_30d, 0),
    mover_score         = CASE WHEN COALESCE(h.sell_median_7d, 0) > 0 AND COALESCE(h.sell_median_30d, 0) > 0
                               THEN ABS((h.sell_median_7d - h.sell_median_30d) / h.sell_median_30d * 100.0) * LN(1.0 + GREATEST(COALESCE(h.isk_traded_7d, 0), 0)) END,
    isk_move_7d         = CASE WHEN COALESCE(h.sell_median_7d, 0) > 0 AND COALESCE(h.sell_median_30d, 0) > 0
                               THEN ABS(h.sell_median_7d - h.sell_median_30d) * GREATEST(COALESCE(h.avg_vol_7d, 0), 0)
                               ELSE 0 END,
    daily_spread_profit = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                               THEN (s.typical_sell - s.typical_buy) * LEAST(GREATEST(COALESCE(h.avg_vol_7d, 0), 0), s.sell_volume::double precision, s.buy_volume::double precision)
                               ELSE 0 END,
    spread_pct          = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                               THEN (s.typical_sell - s.typical_buy) / s.typical_buy * 100.0 END
FROM market_region_stats r
LEFT JOIN hist h ON h.region_id = r.region_id AND h.type_id = r.type_id
LEFT JOIN hold hd ON hd.region_id = r.region_id AND hd.type_id = r.type_id
WHERE s.region_id = r.region_id AND s.type_id = r.type_id;

UPDATE market_region_stats s
SET confirmed_hubs = CASE
    WHEN COALESCE(s.mover_chg_7v30, s.sell_chg_7d) > 0 THEN
        GREATEST(0, COALESCE(c.up_hubs, 0) - CASE WHEN s.typical_sell > 0 AND s.isk_traded_7d > 0 AND COALESCE(s.mover_chg_7v30, s.sell_chg_7d) >= 5.0 THEN 1 ELSE 0 END)
    WHEN COALESCE(s.mover_chg_7v30, s.sell_chg_7d) < 0 THEN
        GREATEST(0, COALESCE(c.down_hubs, 0) - CASE WHEN s.typical_sell > 0 AND s.isk_traded_7d > 0 AND COALESCE(s.mover_chg_7v30, s.sell_chg_7d) <= -5.0 THEN 1 ELSE 0 END)
    ELSE 0
END
FROM (
    SELECT type_id,
           COUNT(*) FILTER (WHERE typical_sell > 0 AND isk_traded_7d > 0 AND COALESCE(mover_chg_7v30, sell_chg_7d) >= 5.0)::bigint AS up_hubs,
           COUNT(*) FILTER (WHERE typical_sell > 0 AND isk_traded_7d > 0 AND COALESCE(mover_chg_7v30, sell_chg_7d) <= -5.0)::bigint AS down_hubs
    FROM market_region_stats
    WHERE region_id IN (10000002, 10000043, 10000032, 10000030, 10000042)
    GROUP BY type_id
) c
WHERE s.type_id = c.type_id
  AND s.region_id IN (10000002, 10000043, 10000032, 10000030, 10000042);

WITH candidates AS (
    SELECT s.region_id, s.type_id, s.isk_traded_7d, s.avg_orders_7d,
           b.sort_by, b.mode, b.period, b.score
    FROM market_region_stats s
    CROSS JOIN LATERAL (
        SELECT 'confirmed'::text AS sort_by, 'rising'::text AS mode, 1::integer AS period, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_1d > 0 THEN ABS(s.sell_chg_1d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END AS score
        UNION ALL SELECT 'confirmed', 'falling',  1,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_1d < 0 THEN ABS(s.sell_chg_1d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'rising',   7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND s.mover_chg_7v30 > 0 THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'falling',  7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND s.mover_chg_7v30 < 0 THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'both',     7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND (s.mover_chg_7v30 > 0 OR s.mover_chg_7v30 < 0) THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'rising',   30, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_30d > 0 THEN ABS(s.sell_chg_30d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'falling',  30, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_30d < 0 THEN ABS(s.sell_chg_30d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'volatile', 0,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.volatility_30d > 0 THEN s.volatility_30d * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'isk_move', 'rising',   1,  CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_1d > 0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_1d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'falling',  1,  CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_1d < 0 AND s.sell_chg_1d > -100.0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_1d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'rising',   7,  CASE WHEN s.avg_vol_7d > 0 AND s.mover_chg_7v30 > 0 AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'falling',  7,  CASE WHEN s.avg_vol_7d > 0 AND s.mover_chg_7v30 < 0 AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'both',     7,  CASE WHEN s.avg_vol_7d > 0 AND (s.mover_chg_7v30 > 0 OR s.mover_chg_7v30 < 0) AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'rising',   30, CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_30d > 0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_30d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'falling',  30, CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_30d < 0 AND s.sell_chg_30d > -100.0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_30d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'volatile', 0,  CASE WHEN s.avg_vol_7d > 0 AND s.volatility_30d > 0 THEN s.typical_sell * s.avg_vol_7d * (s.volatility_30d / 100.0) END
        UNION ALL SELECT 'spread_profit', 'rising',   1,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_1d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  1,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_1d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'rising',   7,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_7d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  7,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_7d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'both',     7,  CASE WHEN s.daily_spread_profit > 0 AND (s.sell_chg_7d > 0 OR s.sell_chg_7d < 0) THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'rising',   30, CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_30d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  30, CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_30d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'volatile', 0,  CASE WHEN s.daily_spread_profit > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'raw_pct', 'rising',   1,  CASE WHEN s.sell_chg_1d > 0 THEN s.sell_chg_1d END
        UNION ALL SELECT 'raw_pct', 'falling',  1,  CASE WHEN s.sell_chg_1d < 0 THEN -s.sell_chg_1d END
        UNION ALL SELECT 'raw_pct', 'rising',   7,  CASE WHEN s.sell_chg_7d > 0 THEN s.sell_chg_7d END
        UNION ALL SELECT 'raw_pct', 'falling',  7,  CASE WHEN s.sell_chg_7d < 0 THEN -s.sell_chg_7d END
        UNION ALL SELECT 'raw_pct', 'both',     7,  CASE WHEN s.sell_chg_7d > 0 OR s.sell_chg_7d < 0 THEN ABS(s.sell_chg_7d) END
        UNION ALL SELECT 'raw_pct', 'rising',   30, CASE WHEN s.sell_chg_30d > 0 THEN s.sell_chg_30d END
        UNION ALL SELECT 'raw_pct', 'falling',  30, CASE WHEN s.sell_chg_30d < 0 THEN -s.sell_chg_30d END
        UNION ALL SELECT 'raw_pct', 'volatile', 0,  CASE WHEN s.volatility_30d > 0 THEN s.volatility_30d END
    ) b
    WHERE s.typical_sell > 0
      AND b.score IS NOT NULL
      AND b.score > 0
),
ranked AS (
    SELECT region_id, sort_by, mode, period, type_id, score, isk_traded_7d, avg_orders_7d,
           ROW_NUMBER() OVER (PARTITION BY region_id, sort_by, mode, period ORDER BY score DESC, type_id ASC)::integer AS rank
    FROM candidates
)
INSERT INTO market_movers_ranked (region_id, sort_by, mode, period, rank, type_id, score, isk_traded_7d, avg_orders_7d, updated_at)
SELECT region_id, sort_by, mode, period, rank, type_id, score, isk_traded_7d, avg_orders_7d, now()
FROM ranked
WHERE sort_by <> 'raw_pct' OR rank <= 100 OR isk_traded_7d > 0
ON CONFLICT (region_id, sort_by, mode, period, rank) DO UPDATE SET
    type_id       = excluded.type_id,
    score         = excluded.score,
    isk_traded_7d = excluded.isk_traded_7d,
    avg_orders_7d = excluded.avg_orders_7d,
    updated_at    = excluded.updated_at;
