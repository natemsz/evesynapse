-- ---------------------------------------------------------------------
-- Dogma fitting data (schema 029, v0.3.21): the full dogma attribute
-- and effect set the fitting simulator computes from, widened from
-- the Fuzzwork dump by the SDE importer (internal/app/sde.go):
--
--   sde_type_attributes  every dgmTypeAttributes row (~1.2M): the
--                        base value of every dogma attribute on
--                        every type. Values arrive in valueInt or
--                        valueFloat; the importer normalizes to
--                        REAL.
--   sde_attribute_types  dgmAttributeTypes: attribute names, the
--                        stackable flag that drives stacking-penalty
--                        grouping, highIsGood, unit, default value.
--   sde_effects          dgmEffects rows that carry a non-empty
--                        modifierInfo (the fitting-relevant set).
--   sde_effect_modifiers the JSON modifiers decoded into rows: one
--                        row per modifier (domain, func, modified /
--                        modifying attribute, operation, plus the
--                        group / required-skill selectors). absent
--                        selectors are stored as 0; modifiers with
--                        no operation (EffectStopper rows) have no
--                        numeric operation and are not stored here.
--   sde_type_effects     dgmTypeEffects: type -> effect links with
--                        isDefault flags.
--
-- Reads stay in sqlc (internal/db/query/queries_sde.sql); the bulk
-- import stays hand-rolled next to the other sde_* tables.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sde_type_attributes (
    type_id      INTEGER NOT NULL,
    attribute_id INTEGER NOT NULL,
    value        REAL    NOT NULL DEFAULT 0,
    PRIMARY KEY (type_id, attribute_id)
);

CREATE INDEX IF NOT EXISTS idx_sde_type_attributes_attr ON sde_type_attributes (attribute_id, type_id);

CREATE TABLE IF NOT EXISTS sde_attribute_types (
    attribute_id  INTEGER PRIMARY KEY,
    name          TEXT    NOT NULL DEFAULT '',
    stackable     INTEGER NOT NULL DEFAULT 1,
    high_is_good  INTEGER NOT NULL DEFAULT 1,
    unit_id       INTEGER NOT NULL DEFAULT 0,
    default_value REAL    NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_effects (
    effect_id INTEGER PRIMARY KEY,
    name      TEXT    NOT NULL DEFAULT '',
    category  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_effect_modifiers (
    effect_id     INTEGER NOT NULL,
    domain        TEXT    NOT NULL DEFAULT '',
    func          TEXT    NOT NULL DEFAULT '',
    modified_attr INTEGER NOT NULL DEFAULT 0,
    modifying_attr INTEGER NOT NULL DEFAULT 0,
    operation     INTEGER NOT NULL DEFAULT 0,
    group_id      INTEGER NOT NULL DEFAULT 0,
    skill_type_id INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_sde_effect_modifiers_effect ON sde_effect_modifiers (effect_id);

CREATE TABLE IF NOT EXISTS sde_type_effects (
    type_id    INTEGER NOT NULL,
    effect_id  INTEGER NOT NULL,
    is_default INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (type_id, effect_id)
);

CREATE INDEX IF NOT EXISTS idx_sde_type_effects_effect ON sde_type_effects (effect_id);

-- Ship mass and per-type volume/capacity ride along from invTypes:
-- dogma has no ship mass attribute (effect modifiers add onto the
-- hull's mass), and drone-bay / cargo checks need type volumes.
-- The fitting engine reads these through the same snapshot.
CREATE TABLE IF NOT EXISTS sde_type_physics (
    type_id  INTEGER PRIMARY KEY,
    mass     REAL    NOT NULL DEFAULT 0,
    volume   REAL    NOT NULL DEFAULT 0,
    capacity REAL    NOT NULL DEFAULT 0
);
