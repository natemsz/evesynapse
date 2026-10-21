-- The universe map (schema 072): New Eden's geometry from the SDE,
-- and the map payload built from it.
--
-- sde_systems gains what mapSolarSystems.csv always carried and the
-- importer dropped: the constellation, the position (metres, the
-- dump's own frame), the owning faction, and the security class.
-- sde_constellations names the middle tier; sde_system_jumps is the
-- stargate graph, one row per direction as the dump lists it.
-- Positions stay 0 until the next import (sde_import_version 11
-- forces one).
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS constellation_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS x DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS y DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS z DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS faction_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sde_systems ADD COLUMN IF NOT EXISTS security_class TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS sde_constellations (
    constellation_id BIGINT PRIMARY KEY,
    name             TEXT NOT NULL,
    region_id        BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sde_system_jumps (
    from_system_id BIGINT NOT NULL,
    to_system_id   BIGINT NOT NULL,
    PRIMARY KEY (from_system_id, to_system_id)
);

-- The map payload: everything the map page draws, built once per
-- import (and again when the layout code's version moves), never at
-- render. One row. sde_build names the dump it came from; version is
-- the builder's (universeMapVersion) so a layout change rebuilds
-- without a re-import; etag is the payload's content hash, which
-- versions its URL.
CREATE TABLE IF NOT EXISTS map_universe (
    id         INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    version    INTEGER NOT NULL,
    etag       TEXT NOT NULL,
    payload    BYTEA NOT NULL,
    systems    INTEGER NOT NULL DEFAULT 0,
    built_at   TIMESTAMPTZ NOT NULL
);
