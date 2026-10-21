-- Place activity (schema 075): ESI's public per-system activity
-- aggregates (ship/NPC/pod kills and ship jumps, published hourly)
-- kept as hourly history, the page-ready figures distilled from
-- that history at ingest, and the current sovereignty holder of
-- every claimed system. The system and region pages read the
-- distilled rows only; nothing is computed at render.
--
-- system_activity_hours is the raw mirror: one row per system per
-- collected hour, pruned to a 30-day retention at ingest. The two
-- stats tables are rebuilt from it on every collection pass: last
-- hour (the newest collected hour, when it is still current),
-- trailing-24h sums, trailing-7d daily averages, and how many
-- hours of history back the averages, so a page can label a
-- partial window honestly.
--
-- system_sovereignty is the current holder set, replaced wholesale
-- each pass: a system with no row is unclaimed. faction_name is
-- stored for faction holders (there is no stored faction-name
-- tier); alliance and corporation names resolve at render through
-- the stored organization records, never from here.
--
-- place_stats_meta gates the collector per dataset on ESI's own
-- cache window, the guide-prices pattern: one row per kind
-- ('activity', 'sovereignty'), the moment of the last successful
-- fetch and the moment the next one is due.

CREATE TABLE IF NOT EXISTS system_activity_hours (
    system_id  BIGINT NOT NULL,
    hour       TIMESTAMPTZ NOT NULL,
    ship_kills BIGINT NOT NULL DEFAULT 0,
    npc_kills  BIGINT NOT NULL DEFAULT 0,
    pod_kills  BIGINT NOT NULL DEFAULT 0,
    ship_jumps BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (system_id, hour)
);

CREATE INDEX IF NOT EXISTS system_activity_hours_hour_idx
    ON system_activity_hours (hour);

CREATE TABLE IF NOT EXISTS system_activity_stats (
    system_id      BIGINT PRIMARY KEY,
    ship_kills_1h  BIGINT NOT NULL DEFAULT 0,
    pod_kills_1h   BIGINT NOT NULL DEFAULT 0,
    npc_kills_1h   BIGINT NOT NULL DEFAULT 0,
    jumps_1h       BIGINT NOT NULL DEFAULT 0,
    ship_kills_24h BIGINT NOT NULL DEFAULT 0,
    pod_kills_24h  BIGINT NOT NULL DEFAULT 0,
    npc_kills_24h  BIGINT NOT NULL DEFAULT 0,
    jumps_24h      BIGINT NOT NULL DEFAULT 0,
    ship_kills_day DOUBLE PRECISION NOT NULL DEFAULT 0,
    pod_kills_day  DOUBLE PRECISION NOT NULL DEFAULT 0,
    npc_kills_day  DOUBLE PRECISION NOT NULL DEFAULT 0,
    jumps_day      DOUBLE PRECISION NOT NULL DEFAULT 0,
    hours_covered  INTEGER NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS region_activity_stats (
    region_id      BIGINT PRIMARY KEY,
    ship_kills_1h  BIGINT NOT NULL DEFAULT 0,
    pod_kills_1h   BIGINT NOT NULL DEFAULT 0,
    npc_kills_1h   BIGINT NOT NULL DEFAULT 0,
    jumps_1h       BIGINT NOT NULL DEFAULT 0,
    ship_kills_24h BIGINT NOT NULL DEFAULT 0,
    pod_kills_24h  BIGINT NOT NULL DEFAULT 0,
    npc_kills_24h  BIGINT NOT NULL DEFAULT 0,
    jumps_24h      BIGINT NOT NULL DEFAULT 0,
    ship_kills_day DOUBLE PRECISION NOT NULL DEFAULT 0,
    pod_kills_day  DOUBLE PRECISION NOT NULL DEFAULT 0,
    npc_kills_day  DOUBLE PRECISION NOT NULL DEFAULT 0,
    jumps_day      DOUBLE PRECISION NOT NULL DEFAULT 0,
    hours_covered  INTEGER NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS system_sovereignty (
    system_id      BIGINT PRIMARY KEY,
    alliance_id    BIGINT NOT NULL DEFAULT 0,
    corporation_id BIGINT NOT NULL DEFAULT 0,
    faction_id     BIGINT NOT NULL DEFAULT 0,
    faction_name   TEXT NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS place_stats_meta (
    kind         TEXT PRIMARY KEY,
    fetched_at   TIMESTAMPTZ NOT NULL,
    cached_until TIMESTAMPTZ NOT NULL
);
