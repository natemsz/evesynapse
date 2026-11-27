-- Schema step 192: indexes for abyssal scoring and lookups, public contract
-- region-cycle sweeps, and incremental LP offer value refreshes.
--
-- All six indexes target small tables (32k-87k rows each; ~5 MB total,
-- sub-second build time). The much larger kill_entity_daily* tables
-- (72M+ rows) are intentionally excluded from synchronous startup DDL.

-- Abyssal item lookups by mutated type and mutaplasmid (recreation paths
-- and selective calibration corpus reads).
CREATE INDEX IF NOT EXISTS abyssal_items_type_idx
    ON abyssal_items (type_id);
CREATE INDEX IF NOT EXISTS abyssal_items_mutator_idx
    ON abyssal_items (mutator_type_id);

-- Live abyssal sightings for the scoring due-queue filter.
CREATE INDEX IF NOT EXISTS abyssal_sightings_live_item_idx
    ON abyssal_sightings (item_id) WHERE departed_at IS NULL;

-- Active public contracts by region and sweep cycle, for sighting refresh
-- and attribution stamping at the end of a region sweep.
CREATE INDEX IF NOT EXISTS pub_contracts_region_cycle_idx
    ON pub_contracts (region_id, last_cycle) WHERE departed_at IS NULL;

-- LP store offer lookups by product or required item type, for incremental
-- value refreshes after each region market sweep.
CREATE INDEX IF NOT EXISTS lp_offers_type_idx
    ON lp_offers (type_id);
CREATE INDEX IF NOT EXISTS lp_offer_requirements_type_idx
    ON lp_offer_requirements (type_id);
