-- Durable name misses (schema 100, esi/name_misses.go): IDs ESI has
-- definitively said are not the thing a name route names (a deleted
-- character's 404, an invalid id's 422, a type id the universe route
-- refuses). The in-process negative caches load these once per
-- process, so a restart no longer re-asks every known-bad ID.
CREATE TABLE IF NOT EXISTS esi_name_misses (
    kind      TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    missed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (kind, entity_id)
);
