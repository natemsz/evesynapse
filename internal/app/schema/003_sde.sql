-- EveSynapse static data (schema addition 003).
-- A local copy of the EVE static data export (SDE), imported from
-- Fuzzwork's CSV conversion of CCP's data dump and refreshed rarely
-- (patch-day cadence). Type/group/station/system names resolve from
-- these tables first; the ESI drip-feed caches (type_names, the
-- in-process maps) remain only as fallback for anything the SDE
-- lacks, notably player-structure names.

CREATE TABLE IF NOT EXISTS sde_types (
    type_id  INTEGER PRIMARY KEY, -- EVE type ID (skill, item, ...)
    name     TEXT    NOT NULL,
    group_id INTEGER NOT NULL DEFAULT 0
);

-- Name lookups (market search) run LIKE over sde_types.
CREATE INDEX IF NOT EXISTS idx_sde_types_name ON sde_types (name);

CREATE TABLE IF NOT EXISTS sde_groups (
    group_id    INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    category_id INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_categories (
    category_id INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS sde_stations (
    station_id INTEGER PRIMARY KEY, -- NPC station ID
    name       TEXT    NOT NULL,
    system_id  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_systems (
    system_id INTEGER PRIMARY KEY,
    name      TEXT    NOT NULL,
    region_id INTEGER NOT NULL DEFAULT 0,
    security  REAL   NOT NULL DEFAULT 0 -- true security status
);

CREATE TABLE IF NOT EXISTS sde_regions (
    region_id INTEGER PRIMARY KEY,
    name      TEXT    NOT NULL
);

-- Import bookkeeping: source base, imported_at, per-file remote
-- markers (ETag / Last-Modified), row totals, last check time.
CREATE TABLE IF NOT EXISTS sde_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
