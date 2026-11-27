-- Schema step 191: station leaderboard band-filtered totals and ranking indexes.
--
-- Adds precomputed open_orders and open_value totals plus composite
-- ranking indexes on market_station_leaderboard so the Top Stations
-- page reads top rows directly in SQL with ORDER BY and LIMIT instead
-- of scanning every station row and sorting in Go.
--
-- Existing rows are backfilled immediately so upgraded databases serve
-- populated totals before the next region sweep replaces each region's
-- open_value with band-filtered figures (counting only orders priced
-- inside each item's Low90 to High90 price band).

ALTER TABLE market_station_leaderboard
    ADD COLUMN IF NOT EXISTS open_orders BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS open_value  DOUBLE PRECISION NOT NULL DEFAULT 0;

UPDATE market_station_leaderboard
SET open_orders = sell_orders + buy_orders,
    open_value  = sell_value + buy_value
WHERE open_orders <> sell_orders + buy_orders
   OR open_value <> sell_value + buy_value;

CREATE INDEX IF NOT EXISTS idx_market_station_leaderboard_value
    ON market_station_leaderboard (open_value DESC, open_orders DESC, location_id ASC);

CREATE INDEX IF NOT EXISTS idx_market_station_leaderboard_region_value
    ON market_station_leaderboard (region_id, open_value DESC, open_orders DESC, location_id ASC);

CREATE INDEX IF NOT EXISTS idx_market_station_leaderboard_orders
    ON market_station_leaderboard (open_orders DESC, open_value DESC, location_id ASC);

CREATE INDEX IF NOT EXISTS idx_market_station_leaderboard_region_orders
    ON market_station_leaderboard (region_id, open_orders DESC, open_value DESC, location_id ASC);
