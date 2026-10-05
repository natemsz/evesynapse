-- ---------------------------------------------------------------------
-- Market station stats (schema 032, P2 spread scanner): the
-- worker's whole-region book sweeps distilled to one row per
-- (station, type): lowest sell price, highest buy price, order
-- counts and remaining volumes at that one place. The spread
-- scanner page reads only these rows; pages never aggregate a
-- book at view time. A completed sweep replaces the region's
-- station rows wholesale inside the same transaction as the
-- region rows, so a station or type that left the book loses
-- its row.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS market_station_stats (
    location_id INTEGER NOT NULL, -- NPC station or player structure ID
    region_id   INTEGER NOT NULL, -- EVE region ID the station sits in
    type_id     INTEGER NOT NULL, -- EVE type ID
    best_sell   REAL    NOT NULL DEFAULT 0, -- lowest sell price here, 0 = none
    best_buy    REAL    NOT NULL DEFAULT 0, -- highest buy price here, 0 = none
    sell_orders INTEGER NOT NULL DEFAULT 0, -- open sell orders counted here
    buy_orders  INTEGER NOT NULL DEFAULT 0, -- open buy orders counted here
    sell_volume INTEGER NOT NULL DEFAULT 0, -- summed volume_remain, sells here
    buy_volume  INTEGER NOT NULL DEFAULT 0, -- summed volume_remain, buys here
    updated_at  TEXT    NOT NULL DEFAULT '', -- RFC3339 sweep completion
    PRIMARY KEY (location_id, type_id)
);

CREATE INDEX IF NOT EXISTS idx_market_station_stats_region ON market_station_stats (region_id, type_id);
CREATE INDEX IF NOT EXISTS idx_market_station_stats_type ON market_station_stats (type_id, region_id);
