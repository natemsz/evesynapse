-- ---------------------------------------------------------------------
-- Market region stats (schema 031, P1 region stats platform): the
-- worker's whole-region book sweeps distilled to one row per
-- (region, type): best and typical (median) prices on both sides,
-- the 9-in-10 bands, order counts and remaining volumes. Rendered
-- by the item page's hub-regions strip and read by every later
-- market phase, so pages never aggregate a book at view time.
-- A completed sweep replaces the region's rows wholesale: a type
-- that left the book loses its row.
--
-- market_region_stats_daily is the same measures snapshotted once
-- per day per pair (upsert by day) -- the raw material for
-- volatility and trend work; nothing renders it yet.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS market_region_stats (
    region_id    INTEGER NOT NULL, -- EVE region ID (10000002 = The Forge)
    type_id      INTEGER NOT NULL, -- EVE type ID
    best_sell    REAL    NOT NULL DEFAULT 0, -- lowest ask, 0 = no sell orders
    typical_sell REAL    NOT NULL DEFAULT 0, -- median sell order price
    sell_band    REAL    NOT NULL DEFAULT 0, -- 9 in 10 sell orders at or under this
    best_buy     REAL    NOT NULL DEFAULT 0, -- highest bid, 0 = no buy orders
    typical_buy  REAL    NOT NULL DEFAULT 0, -- median buy order price
    buy_band     REAL    NOT NULL DEFAULT 0, -- 9 in 10 buy orders at or over this
    sell_orders  INTEGER NOT NULL DEFAULT 0, -- open sell orders counted
    buy_orders   INTEGER NOT NULL DEFAULT 0, -- open buy orders counted
    sell_volume  INTEGER NOT NULL DEFAULT 0, -- summed volume_remain, sells
    buy_volume   INTEGER NOT NULL DEFAULT 0, -- summed volume_remain, buys
    updated_at   TEXT    NOT NULL DEFAULT '', -- RFC3339 sweep completion
    PRIMARY KEY (region_id, type_id)
);

CREATE INDEX IF NOT EXISTS idx_market_region_stats_type ON market_region_stats (type_id, region_id);

CREATE TABLE IF NOT EXISTS market_region_stats_daily (
    region_id    INTEGER NOT NULL,
    type_id      INTEGER NOT NULL,
    day          TEXT    NOT NULL, -- "2006-01-02" (UTC)
    best_sell    REAL    NOT NULL DEFAULT 0,
    typical_sell REAL    NOT NULL DEFAULT 0,
    sell_band    REAL    NOT NULL DEFAULT 0,
    best_buy     REAL    NOT NULL DEFAULT 0,
    typical_buy  REAL    NOT NULL DEFAULT 0,
    buy_band     REAL    NOT NULL DEFAULT 0,
    sell_orders  INTEGER NOT NULL DEFAULT 0,
    buy_orders   INTEGER NOT NULL DEFAULT 0,
    sell_volume  INTEGER NOT NULL DEFAULT 0,
    buy_volume   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (region_id, type_id, day)
);
