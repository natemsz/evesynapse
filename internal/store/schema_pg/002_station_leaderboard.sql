-- EveSynapse schema_pg/002: station leaderboard (P5).
-- One row per (region, station): the open-order counts and the
-- open ISK value per side, distilled by a completed region
-- sweep (market_region_stats.go) from the same staged book as
-- the region and station stats, replaced wholesale inside the
-- sweep completion transaction. The leaderboard page reads
-- only these rows; it never aggregates at render time. Applied
-- by openDB wherever the table is absent: fresh installs get
-- it right after the 001 baseline, existing installs pick it
-- up on their next boot.
CREATE TABLE market_station_leaderboard (
    region_id BIGINT NOT NULL,
    location_id BIGINT NOT NULL,
    sell_orders BIGINT NOT NULL DEFAULT 0,
    buy_orders BIGINT NOT NULL DEFAULT 0,
    sell_value DOUBLE PRECISION NOT NULL DEFAULT 0,
    buy_value DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (region_id, location_id)
);
CREATE INDEX idx_market_station_leaderboard_region ON market_station_leaderboard (region_id, location_id);
