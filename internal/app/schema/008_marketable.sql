-- EveSynapse static data (schema addition 008).
-- Marketability for sde_types: which types can be listed on the
-- market (marketGroupID > 0 in the dump) and whether the type is
-- published at all. Feeds the Market page's live suggestions and
-- the item database's "on market" markers. Existing rows pick up
-- real values on the next SDE import (the importer backfills on
-- first boot after this schema lands; see sdeMaintenance).

ALTER TABLE sde_types ADD COLUMN market_group_id INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sde_types ADD COLUMN published INTEGER NOT NULL DEFAULT 1;

CREATE INDEX IF NOT EXISTS idx_sde_types_marketable ON sde_types (market_group_id, published, name);
