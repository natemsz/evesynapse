-- ---------------------------------------------------------------------
-- The universe map (schema 072): the builder's reads over the SDE
-- geometry, and the one stored payload. Known space only: region IDs
-- from 11000000 up are wormhole space and the abyssal and void
-- pockets, none of which has a place on a stargate map.
-- ---------------------------------------------------------------------

-- name: ListMapSystems :many
SELECT system_id, name, region_id, constellation_id, security,
    x, y, z, position2d_x, position2d_y, faction_id, security_class
FROM sde_systems
WHERE region_id > 0 AND region_id < 11000000
  AND (x <> 0 OR y <> 0 OR z <> 0)
ORDER BY system_id;

-- The systems holding at least one NPC station: a prominence
-- input for the map's label tiers.
-- name: ListMapStationSystems :many
SELECT DISTINCT system_id FROM sde_stations
WHERE system_id > 0
ORDER BY 1;

-- name: ListMapRegions :many
SELECT region_id, name FROM sde_regions
WHERE region_id < 11000000
ORDER BY region_id;

-- name: ListMapConstellations :many
SELECT constellation_id, name, region_id FROM sde_constellations
WHERE region_id < 11000000
ORDER BY constellation_id;

-- Each stargate pair once (the dump lists both directions).
-- name: ListMapJumps :many
SELECT LEAST(from_system_id, to_system_id)::bigint AS a,
    GREATEST(from_system_id, to_system_id)::bigint AS b
FROM sde_system_jumps
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: GetMapUniverseMeta :one
SELECT version, etag, systems, built_at FROM map_universe WHERE id = 1;

-- name: GetMapUniversePayload :one
SELECT etag, payload FROM map_universe WHERE id = 1;

-- name: UpsertMapUniverse :exec
INSERT INTO map_universe (id, version, etag, payload, systems, built_at)
VALUES (1, sqlc.arg(version)::integer, sqlc.arg(etag)::text, sqlc.arg(payload)::bytea,
    sqlc.arg(systems)::integer, sqlc.arg(built_at)::timestamptz)
ON CONFLICT (id) DO UPDATE SET
    version = EXCLUDED.version, etag = EXCLUDED.etag, payload = EXCLUDED.payload,
    systems = EXCLUDED.systems, built_at = EXCLUDED.built_at;

-- The live layers payload (schema 073).
-- name: GetMapLayersMeta :one
SELECT sources, etag, built_at FROM map_layers WHERE id = 1;

-- name: GetMapLayersPayload :one
SELECT etag, payload FROM map_layers WHERE id = 1;

-- name: UpsertMapLayers :exec
INSERT INTO map_layers (id, sources, etag, payload, built_at)
VALUES (1, sqlc.arg(sources)::text, sqlc.arg(etag)::text, sqlc.arg(payload)::bytea,
    sqlc.arg(built_at)::timestamptz)
ON CONFLICT (id) DO UPDATE SET
    sources = EXCLUDED.sources, etag = EXCLUDED.etag,
    payload = EXCLUDED.payload, built_at = EXCLUDED.built_at;

-- ---------------------------------------------------------------------
-- Place activity (schema 075): the collector's gate, the ingest-time
-- distillation into the per-system and per-region figures the place
-- pages read, and the pages' own bounded reads. The hourly history
-- itself is written with set-based inserts in the collector (the
-- guide-prices unnest pattern), not through queries here.
-- ---------------------------------------------------------------------

-- name: GetPlaceStatsMeta :one
SELECT kind, fetched_at, cached_until FROM place_stats_meta
WHERE kind = sqlc.arg(kind)::text;

-- name: UpsertPlaceStatsMeta :exec
INSERT INTO place_stats_meta (kind, fetched_at, cached_until)
VALUES (sqlc.arg(kind)::text, sqlc.arg(fetched_at)::timestamptz, sqlc.arg(cached_until)::timestamptz)
ON CONFLICT (kind) DO UPDATE SET
    fetched_at = EXCLUDED.fetched_at, cached_until = EXCLUDED.cached_until;

-- The 30-day retention, applied at ingest.
-- name: PruneSystemActivityHours :execrows
DELETE FROM system_activity_hours
WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: ClearSystemActivityStats :exec
DELETE FROM system_activity_stats;

-- DistillSystemActivityStats rebuilds the per-system figures from
-- the hourly history as of a moment: the newest collected hour's
-- values count as "last hour" only while that hour is still current
-- (inside two hours of the moment); the 24h columns sum the
-- trailing day; the daily averages scale the trailing 7 days' sums
-- by the hours of collection behind them -- a system missing from
-- a collected hour had no activity that hour, so the denominator
-- is the collection's own coverage (the coverage CTE), which
-- hours_covered reports for the pages' partial-window labels.
-- Systems with no history inside 7 days get no row: their pages
-- show the not-collected state, not stale zeros.
-- name: DistillSystemActivityStats :exec
WITH coverage AS (
    SELECT COUNT(DISTINCT hour)::int AS hours
    FROM system_activity_hours
    WHERE hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
),
latest AS (
    SELECT DISTINCT ON (h.system_id) h.system_id, h.hour,
           h.ship_kills, h.pod_kills, h.npc_kills, h.ship_jumps
    FROM system_activity_hours h
    WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
    ORDER BY h.system_id, h.hour DESC
),
win AS (
    SELECT h.system_id,
           COALESCE(SUM(h.ship_kills) FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS sk24,
           COALESCE(SUM(h.pod_kills)  FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS pk24,
           COALESCE(SUM(h.npc_kills)  FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS nk24,
           COALESCE(SUM(h.ship_jumps) FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS j24,
           SUM(h.ship_kills) AS sk7, SUM(h.pod_kills) AS pk7,
           SUM(h.npc_kills) AS nk7, SUM(h.ship_jumps) AS j7
    FROM system_activity_hours h
    WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
    GROUP BY h.system_id
)
INSERT INTO system_activity_stats (
    system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
    ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
    ship_kills_day, pod_kills_day, npc_kills_day, jumps_day,
    hours_covered, updated_at)
SELECT l.system_id,
       CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.ship_kills ELSE 0 END,
       CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.pod_kills ELSE 0 END,
       CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.npc_kills ELSE 0 END,
       CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.ship_jumps ELSE 0 END,
       COALESCE(w.sk24, 0), COALESCE(w.pk24, 0), COALESCE(w.nk24, 0), COALESCE(w.j24, 0),
       COALESCE(w.sk7, 0) * 24.0 / GREATEST(c.hours, 1),
       COALESCE(w.pk7, 0) * 24.0 / GREATEST(c.hours, 1),
       COALESCE(w.nk7, 0) * 24.0 / GREATEST(c.hours, 1),
       COALESCE(w.j7, 0) * 24.0 / GREATEST(c.hours, 1),
       c.hours,
       sqlc.arg(now)::timestamptz
FROM latest l
CROSS JOIN coverage c
LEFT JOIN win w ON w.system_id = l.system_id;

-- name: ClearRegionActivityStats :exec
DELETE FROM region_activity_stats;

-- DistillRegionActivityStats is DistillSystemActivityStats summed
-- over each region's member systems: the 1h and window sums add the
-- systems' histories directly, and the averages and hours_covered
-- use the same collection coverage, so a region's daily average is
-- its total scaled by the hours of collection behind it.
-- name: DistillRegionActivityStats :exec
WITH coverage AS (
    SELECT COUNT(DISTINCT hour)::int AS hours
    FROM system_activity_hours
    WHERE hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
),
latest AS (
    SELECT DISTINCT ON (h.system_id) h.system_id, h.hour,
           h.ship_kills, h.pod_kills, h.npc_kills, h.ship_jumps
    FROM system_activity_hours h
    WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
    ORDER BY h.system_id, h.hour DESC
),
reg1h AS (
    SELECT s.region_id,
           SUM(CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.ship_kills ELSE 0 END) AS sk1,
           SUM(CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.pod_kills ELSE 0 END) AS pk1,
           SUM(CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.npc_kills ELSE 0 END) AS nk1,
           SUM(CASE WHEN l.hour > sqlc.arg(now)::timestamptz - INTERVAL '2 hours' THEN l.ship_jumps ELSE 0 END) AS j1
    FROM latest l
    JOIN sde_systems s ON s.system_id = l.system_id
    GROUP BY s.region_id
),
regwin AS (
    SELECT s.region_id,
           COALESCE(SUM(h.ship_kills) FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS sk24,
           COALESCE(SUM(h.pod_kills)  FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS pk24,
           COALESCE(SUM(h.npc_kills)  FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS nk24,
           COALESCE(SUM(h.ship_jumps) FILTER (WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '24 hours'), 0) AS j24,
           SUM(h.ship_kills) AS sk7, SUM(h.pod_kills) AS pk7,
           SUM(h.npc_kills) AS nk7, SUM(h.ship_jumps) AS j7
    FROM system_activity_hours h
    JOIN sde_systems s ON s.system_id = h.system_id
    WHERE h.hour > sqlc.arg(now)::timestamptz - INTERVAL '7 days'
    GROUP BY s.region_id
)
INSERT INTO region_activity_stats (
    region_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
    ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
    ship_kills_day, pod_kills_day, npc_kills_day, jumps_day,
    hours_covered, updated_at)
SELECT r.region_id, r.sk1, r.pk1, r.nk1, r.j1,
       w.sk24, w.pk24, w.nk24, w.j24,
       w.sk7 * 24.0 / GREATEST(c.hours, 1),
       w.pk7 * 24.0 / GREATEST(c.hours, 1),
       w.nk7 * 24.0 / GREATEST(c.hours, 1),
       w.j7 * 24.0 / GREATEST(c.hours, 1),
       c.hours,
       sqlc.arg(now)::timestamptz
FROM reg1h r
CROSS JOIN coverage c
JOIN regwin w ON w.region_id = r.region_id;

-- name: ClearSystemSovereignty :exec
DELETE FROM system_sovereignty;

-- The previous pass's faction names, so a pass whose factions fetch
-- failed keeps the names it already had instead of blanking them.
-- name: ListSystemSovereigntyFactionNames :many
SELECT system_id, faction_name FROM system_sovereignty
WHERE faction_id > 0 AND faction_name <> '';

-- name: GetSystemActivityStats :one
SELECT system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
       ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
       ship_kills_day, pod_kills_day, npc_kills_day, jumps_day,
       hours_covered, updated_at
FROM system_activity_stats
WHERE system_id = sqlc.arg(system_id)::bigint;

-- The same distilled figures for a set of systems at once: a
-- system's neighbours, for the workbench's gate cards.
-- name: ListSystemActivityStatsByIDs :many
SELECT system_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
       ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
       ship_kills_day, pod_kills_day, npc_kills_day, jumps_day,
       hours_covered, updated_at
FROM system_activity_stats
WHERE system_id = ANY(sqlc.arg(system_ids)::bigint[]);

-- name: GetRegionActivityStats :one
SELECT region_id, ship_kills_1h, pod_kills_1h, npc_kills_1h, jumps_1h,
       ship_kills_24h, pod_kills_24h, npc_kills_24h, jumps_24h,
       ship_kills_day, pod_kills_day, npc_kills_day, jumps_day,
       hours_covered, updated_at
FROM region_activity_stats
WHERE region_id = sqlc.arg(region_id)::bigint;

-- name: GetSystemSovereignty :one
SELECT system_id, alliance_id, corporation_id, faction_id, faction_name, updated_at
FROM system_sovereignty
WHERE system_id = sqlc.arg(system_id)::bigint;

-- ListRegionSystemsWithActivity: one region's systems for its page,
-- with each system's trailing-24h kills and jumps from the distilled
-- stats (0 until collection reaches the system).
-- name: ListRegionSystemsWithActivity :many
SELECT s.system_id, s.name, s.security,
       COALESCE(st.ship_kills_24h, 0) AS ship_kills_24h,
       COALESCE(st.jumps_24h, 0) AS jumps_24h
FROM sde_systems s
LEFT JOIN system_activity_stats st ON st.system_id = s.system_id
WHERE s.region_id = sqlc.arg(region_id)::bigint
ORDER BY s.name;

-- ListRegionSovereigntyBreakdown: a region's current holders by
-- systems held, most first; the caller bounds the list.
-- name: ListRegionSovereigntyBreakdown :many
SELECT sov.alliance_id, sov.corporation_id, sov.faction_id, sov.faction_name,
       COUNT(*)::bigint AS systems
FROM system_sovereignty sov
JOIN sde_systems s ON s.system_id = sov.system_id
WHERE s.region_id = sqlc.arg(region_id)::bigint
GROUP BY sov.alliance_id, sov.corporation_id, sov.faction_id, sov.faction_name
ORDER BY systems DESC, sov.alliance_id, sov.corporation_id, sov.faction_id
LIMIT sqlc.arg(lim)::bigint;
