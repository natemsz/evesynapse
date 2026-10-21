-- The universe map's live layers (schema 073): kills and jumps in
-- the last hour, sovereignty, Faction Warfare and incursions, built
-- by the worker from the stored public snapshots into one payload
-- and stored, never assembled at render. One row. sources
-- fingerprints what it was built from (each snapshot's fetch time
-- and the holding alliances' resolved names), so a pass rebuilds
-- only when something moved; etag is the payload's content hash,
-- which versions its URL.
CREATE TABLE IF NOT EXISTS map_layers (
    id       INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    sources  TEXT NOT NULL,
    etag     TEXT NOT NULL,
    payload  BYTEA NOT NULL,
    built_at TIMESTAMPTZ NOT NULL
);
