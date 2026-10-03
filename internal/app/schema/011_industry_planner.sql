-- Migration 011 (Phase 3): the industry build planner's static
-- data — what blueprint makes what, from what, and at what base
-- time. Imported from the same Fuzzwork dump as schema 003's
-- tables (industryBlueprints / industryActivity /
-- industryActivityProducts / industryActivityMaterials /
-- industryActivitySkills) by the SDE importer; only the
-- manufacturing activity (activityID 1) is kept.

CREATE TABLE IF NOT EXISTS sde_blueprints (
    blueprint_type_id         INTEGER PRIMARY KEY, -- EVE type ID of the blueprint itself
    product_type_id           INTEGER NOT NULL,    -- EVE type ID one run produces
    product_quantity          INTEGER NOT NULL DEFAULT 1, -- units per run (ammo etc. produce more)
    max_production_limit      INTEGER NOT NULL DEFAULT 0, -- from industryBlueprints.csv
    manufacturing_time_seconds INTEGER NOT NULL DEFAULT 0 -- base seconds per run, before TE
);

-- The planner walks product -> blueprint, not just blueprint ->
-- product, so the reverse lookup is indexed too.
CREATE INDEX IF NOT EXISTS idx_sde_blueprints_product ON sde_blueprints (product_type_id);

CREATE TABLE IF NOT EXISTS sde_blueprint_materials (
    blueprint_type_id INTEGER NOT NULL,
    material_type_id  INTEGER NOT NULL,
    quantity          INTEGER NOT NULL, -- base units consumed per run, before ME
    PRIMARY KEY (blueprint_type_id, material_type_id)
);

CREATE TABLE IF NOT EXISTS sde_blueprint_skills (
    blueprint_type_id INTEGER NOT NULL,
    skill_type_id     INTEGER NOT NULL,
    level             INTEGER NOT NULL, -- required trained level (1-5)
    PRIMARY KEY (blueprint_type_id, skill_type_id)
);
