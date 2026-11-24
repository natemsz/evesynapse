-- Step 179: factions and planetary industry schematics from the static
-- data. ESI no longer serves either; the importer fills these from the
-- SDE API (or an uploaded dump).
CREATE TABLE IF NOT EXISTS sde_factions (
    faction_id             BIGINT PRIMARY KEY,
    name                   TEXT   NOT NULL,
    corporation_id         BIGINT NOT NULL DEFAULT 0,
    militia_corporation_id BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_schematics (
    schematic_id BIGINT PRIMARY KEY,
    name         TEXT   NOT NULL,
    cycle_time   BIGINT NOT NULL DEFAULT 0
);
