-- Schema step 182: gate camps per system, stored at ingest.
--
-- The map's threat layer finds the camps (map_threat.go buildThreat:
-- one group on most of a system's recent public ship kills, spread
-- over time and over several victim corporations; structures and
-- deployables never count). Each layer build replaces this table
-- with what it found, so every surface reads one stored answer
-- instead of finding camps again: the home's Radar
-- widget, the Briefing's camp card and the camp notifications.
-- detected_at is the build's moment: readers ignore rows older than
-- a few builds, so a stalled worker shows no camp rather than an old
-- one.
CREATE TABLE IF NOT EXISTS system_camps (
    system_id   BIGINT PRIMARY KEY,
    group_kind  TEXT NOT NULL,
    group_id    BIGINT NOT NULL,
    group_name  TEXT NOT NULL DEFAULT '',
    kills       INTEGER NOT NULL,
    since       TIMESTAMPTZ NOT NULL,
    celestial   TEXT NOT NULL DEFAULT '',
    spread      BOOLEAN NOT NULL DEFAULT FALSE,
    crew        TEXT[] NOT NULL DEFAULT '{}',
    detected_at TIMESTAMPTZ NOT NULL
);
