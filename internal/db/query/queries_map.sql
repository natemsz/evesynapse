-- ---------------------------------------------------------------------
-- The universe map (schema 072): the builder's reads over the SDE
-- geometry, and the one stored payload. Known space only: region IDs
-- from 11000000 up are wormhole space and the abyssal and void
-- pockets, none of which has a place on a stargate map.
-- ---------------------------------------------------------------------

-- name: ListMapSystems :many
SELECT system_id, name, region_id, constellation_id, security,
    x, y, z, faction_id, security_class
FROM sde_systems
WHERE region_id > 0 AND region_id < 11000000
  AND (x <> 0 OR y <> 0 OR z <> 0)
ORDER BY system_id;

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
