-- ---------------------------------------------------------------------
-- Structure context (schema 028, v0.3.16): what corporation
-- structure snapshots know about each player structure -- owning
-- corporation, solar system, structure type -- persisted when the
-- snapshots are processed, so the structure page can render real
-- context without re-reading snapshot payloads. Names stay in
-- structure_names under their provenance rules; this table only
-- carries facts the owning corporation's own list reported.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS structure_context (
    structure_id         INTEGER PRIMARY KEY,
    owner_corporation_id INTEGER NOT NULL DEFAULT 0,
    system_id            INTEGER NOT NULL DEFAULT 0,
    type_id              INTEGER NOT NULL DEFAULT 0,
    updated_at           TEXT NOT NULL DEFAULT ''
);
