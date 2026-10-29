-- Schema step 117: contract sales and invention in profit per
-- blueprint.
--
-- Invention lives in the SDE's industry files under activityID 8: the
-- T2 blueprint copy a T1 blueprint's invention makes (runs per copy)
-- and the datacores each attempt takes. The importer keeps them now
-- (import version 14 re-imports).
--
-- Completed item exchange contracts a character issued (not for its
-- corporation) take their included items out of the pool's stock when
-- they complete. ESI prices a contract as a whole, so the price is
-- split across its lines by the house value of each, stamped as split;
-- a contract whose lines have no house value splits by quantity.

CREATE TABLE IF NOT EXISTS sde_invention (
    blueprint_type_id BIGINT NOT NULL,
    product_type_id   BIGINT NOT NULL,
    runs              BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (blueprint_type_id, product_type_id)
);
CREATE INDEX IF NOT EXISTS sde_invention_product_idx ON sde_invention (product_type_id);

CREATE TABLE IF NOT EXISTS sde_invention_materials (
    blueprint_type_id BIGINT NOT NULL,
    material_type_id  BIGINT NOT NULL,
    quantity          BIGINT NOT NULL,
    PRIMARY KEY (blueprint_type_id, material_type_id)
);

CREATE TABLE IF NOT EXISTS ledger_contract_items (
    owner_kind  TEXT NOT NULL,
    owner_id    BIGINT NOT NULL,
    contract_id BIGINT NOT NULL,
    type_id     BIGINT NOT NULL,
    at          TIMESTAMPTZ NOT NULL,
    quantity    BIGINT NOT NULL,
    price_share DOUBLE PRECISION NOT NULL, -- this line's part of the contract price
    split_by    TEXT NOT NULL,             -- 'value' | 'quantity'
    PRIMARY KEY (owner_kind, owner_id, contract_id, type_id)
);
CREATE INDEX IF NOT EXISTS ledger_contract_items_type_idx ON ledger_contract_items (owner_kind, type_id, at);

-- What invention charged a T2 blueprint's runs.
ALTER TABLE industry_bp_summary ADD COLUMN IF NOT EXISTS invention_cost DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE industry_bp_summary ADD COLUMN IF NOT EXISTS invention_per_run DOUBLE PRECISION NOT NULL DEFAULT 0;
-- 'none' (not an invented blueprint), 'found' (the pool's own
-- inventions price it) or 'missing' (invented, but no invention of it
-- in the pool's history: copies bought, or invented before it).
ALTER TABLE industry_bp_summary ADD COLUMN IF NOT EXISTS invention TEXT NOT NULL DEFAULT 'none';

-- Delivered invention jobs and completed contracts land on the next
-- pass; every pool refolds.
UPDATE ledger_state SET jobs_hash = '';
UPDATE ledger_pool_state SET members = '';
