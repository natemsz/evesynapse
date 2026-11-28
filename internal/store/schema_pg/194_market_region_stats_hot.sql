-- Schema step 194: let market_region_stats rows update in place.
--
-- Every sweep rewrites rows of market_region_stats. PostgreSQL can
-- update a row inside its own page (a HOT update) only when no index
-- covers a changed column and the page has free room. The movers
-- index covers isk_traded_7d and mover_score, which most sweeps
-- change, and nothing needs it: the queries that filter on
-- isk_traded_7d also filter region_id, which the primary key serves.
-- Dropping it and leaving a fifth of each page free lets most sweep
-- updates stay in place instead of growing the table and every index.

DROP INDEX IF EXISTS market_region_stats_movers_idx;

ALTER TABLE market_region_stats SET (fillfactor = 80);
