-- Schema step 145: the game's buff tables, what a command burst does.
--
-- A burst's charge names buffs by id; each buff has an operation, an
-- aggregate mode (when several bursts carry the same buff the
-- highest, or the lowest, value holds) and the attributes it changes:
-- on the ship itself (kind item), on everything fitted (location),
-- on fitted modules of a group (group) or requiring a skill (skill).
CREATE TABLE IF NOT EXISTS sde_dbuffs (
    dbuff_id     BIGINT PRIMARY KEY,
    display_name TEXT   NOT NULL DEFAULT '',
    aggregate    TEXT   NOT NULL DEFAULT '',
    operation    TEXT   NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS sde_dbuff_modifiers (
    dbuff_id     BIGINT NOT NULL,
    kind         TEXT   NOT NULL,
    attribute_id BIGINT NOT NULL,
    group_id     BIGINT NOT NULL DEFAULT 0,
    skill_id     BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sde_dbuff_modifiers_dbuff_idx ON sde_dbuff_modifiers (dbuff_id);
