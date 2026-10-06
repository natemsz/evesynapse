-- Schema step 004 (v0.3.33): per-type market price TTL cache and
-- industry cost index tracking.
--
-- market_type_prices: per-(region, type) best buy/sell with fetch
-- timestamp. The worker refreshes only rows older than the TTL
-- (5 minutes); pages read cache-only. This is the stale-only
-- refresh pattern from EVE-Nexus, cheaper than full sweeps for
-- user-triggered price needs (planner, shopping lists).
--
-- industry_cost_indices: ESI GET /industry/systems/ cost indices per
-- (solar_system_id, activity). Refreshed by the worker on its own
-- schedule; the planner and industry pages read stored values.

CREATE TABLE IF NOT EXISTS market_type_prices (
    region_id   BIGINT NOT NULL,
    type_id     BIGINT NOT NULL,
    buy_price   DOUBLE PRECISION NOT NULL DEFAULT 0,
    sell_price  DOUBLE PRECISION NOT NULL DEFAULT 0,
    buy_volume  BIGINT NOT NULL DEFAULT 0,
    sell_volume BIGINT NOT NULL DEFAULT 0,
    fetched_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region_id, type_id)
);
CREATE INDEX IF NOT EXISTS idx_market_type_prices_fetched
    ON market_type_prices (fetched_at);

CREATE TABLE IF NOT EXISTS industry_cost_indices (
    solar_system_id BIGINT NOT NULL,
    activity        TEXT NOT NULL,
    cost_index      DOUBLE PRECISION NOT NULL,
    fetched_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (solar_system_id, activity)
);
CREATE INDEX IF NOT EXISTS idx_industry_cost_indices_fetched
    ON industry_cost_indices (fetched_at);
