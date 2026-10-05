-- ---------------------------------------------------------------------
-- Market sweep staging (schema 034, P1 sweep upgrade): the
-- whole-region book sweeps stage every fetched page on disk as it
-- lands, so a sweep survives process restarts and updates instead
-- of restarting from page 1. One market_sweep_state row per
-- region in mid-sweep: its presence IS the sweep, and next_page
-- is the resume cursor. A page's orders and the cursor advance
-- commit in one transaction, so market_sweep_orders always holds
-- exactly the pages already read of the sweep the state row
-- describes. When a book is fully staged it is distilled into
-- market_region_stats / market_station_stats and the staging is
-- deleted in the same transaction, so previous stats keep
-- serving until the new ones land whole.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS market_sweep_state (
    region_id   INTEGER NOT NULL, -- EVE region ID
    next_page   INTEGER NOT NULL DEFAULT 1, -- next page to fetch (1-based)
    pages_total INTEGER NOT NULL DEFAULT 0, -- book size from X-Pages, 0 until page 1 answers
    started_at  TEXT    NOT NULL DEFAULT '', -- RFC3339 sweep start
    updated_at  TEXT    NOT NULL DEFAULT '', -- RFC3339 last staged page
    PRIMARY KEY (region_id)
);

CREATE TABLE IF NOT EXISTS market_sweep_orders (
    region_id     INTEGER NOT NULL, -- EVE region ID
    type_id       INTEGER NOT NULL, -- EVE type ID
    is_buy_order  INTEGER NOT NULL DEFAULT 0, -- 1 = buy, 0 = sell
    price         REAL    NOT NULL DEFAULT 0, -- ISK
    volume_remain INTEGER NOT NULL DEFAULT 0, -- units still open
    location_id   INTEGER NOT NULL DEFAULT 0 -- station or structure ID, 0 when ESI reports none
);

CREATE INDEX IF NOT EXISTS idx_market_sweep_orders_agg ON market_sweep_orders (region_id, type_id, is_buy_order, price);
