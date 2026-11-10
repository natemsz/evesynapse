-- Schema step 137: drop the region sweep's disk staging.
--
-- The region sweep reads a region's whole book inside one ESI
-- cache window and stores it from memory, comparing it with the
-- previous sweep's book held in memory, so nothing stages pages
-- on disk or keeps a resume cursor any more.
DROP TABLE IF EXISTS market_sweep_orders;
DROP TABLE IF EXISTS market_sweep_state;
