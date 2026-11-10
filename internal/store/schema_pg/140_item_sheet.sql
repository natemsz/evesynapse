-- Schema step 140: what an item sheet needs to show a type's stats.
--
-- The dump's attribute metadata gains the columns the game uses to
-- show an attribute: its display name, whether it is shown at all,
-- whether a zero is still shown, its section and its icon. Units
-- name each attribute's unit. Traits are the ship and subsystem
-- bonuses (skill_id -1 is a role bonus, any other id a bonus per
-- level of that skill). Meta types link a variation to its base
-- type, and meta groups name the variation tiers.
ALTER TABLE sde_attribute_types
    ADD COLUMN IF NOT EXISTS display_name      TEXT   NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS published         BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS display_when_zero BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS category_id       BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS icon_id           BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS sde_units (
    unit_id      BIGINT PRIMARY KEY,
    display_name TEXT   NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sde_traits (
    trait_id   BIGINT           PRIMARY KEY,
    type_id    BIGINT           NOT NULL,
    skill_id   BIGINT           NOT NULL,
    bonus      DOUBLE PRECISION,
    bonus_text TEXT             NOT NULL DEFAULT '',
    unit_id    BIGINT           NOT NULL DEFAULT 0,
    importance BIGINT           NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sde_traits_type_idx ON sde_traits (type_id);

CREATE TABLE IF NOT EXISTS sde_meta_types (
    type_id        BIGINT PRIMARY KEY,
    parent_type_id BIGINT NOT NULL DEFAULT 0,
    meta_group_id  BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sde_meta_types_parent_idx ON sde_meta_types (parent_type_id);

CREATE TABLE IF NOT EXISTS sde_meta_groups (
    meta_group_id BIGINT PRIMARY KEY,
    name          TEXT   NOT NULL DEFAULT ''
);
