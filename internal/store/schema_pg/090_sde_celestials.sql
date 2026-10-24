-- Celestials (schema 090): the SDE's named places inside a
-- system -- suns, planets, moons, asteroid belts and stargates --
-- from mapDenormalize.csv, only the groups a kill's location can
-- point at. kills.location_id is one of these item IDs (zKill's
-- nearest celestial for the victim), and this table is what
-- turns it back into a name: a stargate's name carries its
-- destination ("Stargate (Hrokkur)"), so a gate camp can name
-- its gate. Stations are not kept here; sde_stations names
-- those. Rows land with the next SDE import (sde_import_version
-- 13 forces one).
CREATE TABLE IF NOT EXISTS sde_celestials (
    item_id   BIGINT PRIMARY KEY,
    system_id BIGINT NOT NULL DEFAULT 0,
    name      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sde_celestials_system_idx ON sde_celestials (system_id);
