-- EveSynapse snapshot cache (schema addition 002).
-- Raw ESI payloads cached per character + endpoint kind, honoring the
-- Expires header ESI returns (cached_until). Type names cache the
-- /universe/types/{id}/ lookups used to label skill IDs.

CREATE TABLE character_snapshots (
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL, -- "skills" | "skillqueue" | "wallet" | "assets"
    payload      TEXT    NOT NULL, -- raw ESI JSON body
    fetched_at   TEXT    NOT NULL, -- RFC3339
    cached_until TEXT,             -- RFC3339, from the ESI Expires header
    PRIMARY KEY (character_id, kind)
);

CREATE TABLE type_names (
    type_id INTEGER PRIMARY KEY, -- EVE type ID (skill, item, ...)
    name    TEXT    NOT NULL
);
