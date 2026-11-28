-- Schema step 193: the planner prices from market_region_stats, so the
-- per-region price cache is dropped.
DROP TABLE IF EXISTS market_type_prices;
