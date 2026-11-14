-- The killboard's filtered lists (killboard_filter.go). A filtered
-- page never walks a scope's whole history: each query takes at most
-- scan_cap of the scope's newest kills before the cursor, through the
-- scope's own index, and keeps the ones that pass the filters. When
-- a page comes up short, the matching KillScanEdge query names the
-- last kill the window read, so the next page starts behind it.
-- Security bands: abyssal and wormhole space by system id, Pochven
-- by region, then high (0.45 and up), low (above 0) and null.

-- name: ListKillsFilteredRecent :many
WITH cand AS MATERIALIZED (
    SELECT k.killmail_id FROM kills k WHERE k.public AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
    ORDER BY k.killmail_time DESC, k.killmail_id DESC
    LIMIT sqlc.arg(scan_cap)::bigint
)
SELECT k.* FROM cand c
JOIN kills k ON k.killmail_id = c.killmail_id
LEFT JOIN sde_systems ss ON ss.system_id = k.system_id
LEFT JOIN sde_types st ON st.type_id = k.ship_type_id
WHERE k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
  AND k.attacker_count >= sqlc.arg(min_attackers)::integer
  AND (sqlc.arg(max_attackers)::integer = 0 OR k.attacker_count <= sqlc.arg(max_attackers)::integer)
  AND (sqlc.arg(system_id)::bigint = 0 OR k.system_id = sqlc.arg(system_id)::bigint)
  AND (sqlc.arg(region_id)::bigint = 0 OR k.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(ship_type_id)::bigint = 0 OR k.ship_type_id = sqlc.arg(ship_type_id)::bigint)
  AND (sqlc.arg(group_id)::bigint = 0 OR st.group_id = sqlc.arg(group_id)::bigint)
  AND (sqlc.arg(band)::text = '' OR CASE WHEN k.system_id >= 32000000 THEN 'abyssal'
                WHEN k.system_id >= 31000000 THEN 'wh'
                WHEN k.region_id = 10000070 THEN 'pochven'
                WHEN COALESCE(ss.security, 0) >= 0.45 THEN 'high'
                WHEN COALESCE(ss.security, 0) > 0 THEN 'low'
                ELSE 'null' END = sqlc.arg(band)::text)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: KillScanEdgeRecent :one
SELECT k.killmail_time, k.killmail_id FROM kills k WHERE k.public AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
OFFSET sqlc.arg(scan_cap)::bigint - 1
LIMIT 1;

-- name: ListKillsFilteredSystem :many
WITH cand AS MATERIALIZED (
    SELECT k.killmail_id FROM kills k WHERE k.public AND k.system_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
    ORDER BY k.killmail_time DESC, k.killmail_id DESC
    LIMIT sqlc.arg(scan_cap)::bigint
)
SELECT k.* FROM cand c
JOIN kills k ON k.killmail_id = c.killmail_id
LEFT JOIN sde_systems ss ON ss.system_id = k.system_id
LEFT JOIN sde_types st ON st.type_id = k.ship_type_id
WHERE k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
  AND k.attacker_count >= sqlc.arg(min_attackers)::integer
  AND (sqlc.arg(max_attackers)::integer = 0 OR k.attacker_count <= sqlc.arg(max_attackers)::integer)
  AND (sqlc.arg(system_id)::bigint = 0 OR k.system_id = sqlc.arg(system_id)::bigint)
  AND (sqlc.arg(region_id)::bigint = 0 OR k.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(ship_type_id)::bigint = 0 OR k.ship_type_id = sqlc.arg(ship_type_id)::bigint)
  AND (sqlc.arg(group_id)::bigint = 0 OR st.group_id = sqlc.arg(group_id)::bigint)
  AND (sqlc.arg(band)::text = '' OR CASE WHEN k.system_id >= 32000000 THEN 'abyssal'
                WHEN k.system_id >= 31000000 THEN 'wh'
                WHEN k.region_id = 10000070 THEN 'pochven'
                WHEN COALESCE(ss.security, 0) >= 0.45 THEN 'high'
                WHEN COALESCE(ss.security, 0) > 0 THEN 'low'
                ELSE 'null' END = sqlc.arg(band)::text)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: KillScanEdgeSystem :one
SELECT k.killmail_time, k.killmail_id FROM kills k WHERE k.public AND k.system_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
OFFSET sqlc.arg(scan_cap)::bigint - 1
LIMIT 1;

-- name: ListKillsFilteredRegion :many
WITH cand AS MATERIALIZED (
    SELECT k.killmail_id FROM kills k WHERE k.public AND k.region_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
    ORDER BY k.killmail_time DESC, k.killmail_id DESC
    LIMIT sqlc.arg(scan_cap)::bigint
)
SELECT k.* FROM cand c
JOIN kills k ON k.killmail_id = c.killmail_id
LEFT JOIN sde_systems ss ON ss.system_id = k.system_id
LEFT JOIN sde_types st ON st.type_id = k.ship_type_id
WHERE k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
  AND k.attacker_count >= sqlc.arg(min_attackers)::integer
  AND (sqlc.arg(max_attackers)::integer = 0 OR k.attacker_count <= sqlc.arg(max_attackers)::integer)
  AND (sqlc.arg(system_id)::bigint = 0 OR k.system_id = sqlc.arg(system_id)::bigint)
  AND (sqlc.arg(region_id)::bigint = 0 OR k.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(ship_type_id)::bigint = 0 OR k.ship_type_id = sqlc.arg(ship_type_id)::bigint)
  AND (sqlc.arg(group_id)::bigint = 0 OR st.group_id = sqlc.arg(group_id)::bigint)
  AND (sqlc.arg(band)::text = '' OR CASE WHEN k.system_id >= 32000000 THEN 'abyssal'
                WHEN k.system_id >= 31000000 THEN 'wh'
                WHEN k.region_id = 10000070 THEN 'pochven'
                WHEN COALESCE(ss.security, 0) >= 0.45 THEN 'high'
                WHEN COALESCE(ss.security, 0) > 0 THEN 'low'
                ELSE 'null' END = sqlc.arg(band)::text)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: KillScanEdgeRegion :one
SELECT k.killmail_time, k.killmail_id FROM kills k WHERE k.public AND k.region_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
OFFSET sqlc.arg(scan_cap)::bigint - 1
LIMIT 1;

-- name: ListKillsFilteredShip :many
WITH cand AS MATERIALIZED (
    SELECT k.killmail_id FROM kills k WHERE k.public AND k.ship_type_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
    ORDER BY k.killmail_time DESC, k.killmail_id DESC
    LIMIT sqlc.arg(scan_cap)::bigint
)
SELECT k.* FROM cand c
JOIN kills k ON k.killmail_id = c.killmail_id
LEFT JOIN sde_systems ss ON ss.system_id = k.system_id
LEFT JOIN sde_types st ON st.type_id = k.ship_type_id
WHERE k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
  AND k.attacker_count >= sqlc.arg(min_attackers)::integer
  AND (sqlc.arg(max_attackers)::integer = 0 OR k.attacker_count <= sqlc.arg(max_attackers)::integer)
  AND (sqlc.arg(system_id)::bigint = 0 OR k.system_id = sqlc.arg(system_id)::bigint)
  AND (sqlc.arg(region_id)::bigint = 0 OR k.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(ship_type_id)::bigint = 0 OR k.ship_type_id = sqlc.arg(ship_type_id)::bigint)
  AND (sqlc.arg(group_id)::bigint = 0 OR st.group_id = sqlc.arg(group_id)::bigint)
  AND (sqlc.arg(band)::text = '' OR CASE WHEN k.system_id >= 32000000 THEN 'abyssal'
                WHEN k.system_id >= 31000000 THEN 'wh'
                WHEN k.region_id = 10000070 THEN 'pochven'
                WHEN COALESCE(ss.security, 0) >= 0.45 THEN 'high'
                WHEN COALESCE(ss.security, 0) > 0 THEN 'low'
                ELSE 'null' END = sqlc.arg(band)::text)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: KillScanEdgeShip :one
SELECT k.killmail_time, k.killmail_id FROM kills k WHERE k.public AND k.ship_type_id = sqlc.arg(scope_id)::bigint AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
OFFSET sqlc.arg(scan_cap)::bigint - 1
LIMIT 1;

-- name: ListKillsFilteredEntity :many
WITH cand AS MATERIALIZED (
    SELECT f.killmail_id FROM kill_entity_feed f WHERE f.kind = sqlc.arg(kind)::text AND f.entity_id = sqlc.arg(scope_id)::bigint AND (f.killmail_time, f.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
    ORDER BY f.killmail_time DESC, f.killmail_id DESC
    LIMIT sqlc.arg(scan_cap)::bigint
)
SELECT k.* FROM cand c
JOIN kills k ON k.killmail_id = c.killmail_id
LEFT JOIN sde_systems ss ON ss.system_id = k.system_id
LEFT JOIN sde_types st ON st.type_id = k.ship_type_id
WHERE k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
  AND k.attacker_count >= sqlc.arg(min_attackers)::integer
  AND (sqlc.arg(max_attackers)::integer = 0 OR k.attacker_count <= sqlc.arg(max_attackers)::integer)
  AND (sqlc.arg(system_id)::bigint = 0 OR k.system_id = sqlc.arg(system_id)::bigint)
  AND (sqlc.arg(region_id)::bigint = 0 OR k.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(ship_type_id)::bigint = 0 OR k.ship_type_id = sqlc.arg(ship_type_id)::bigint)
  AND (sqlc.arg(group_id)::bigint = 0 OR st.group_id = sqlc.arg(group_id)::bigint)
  AND (sqlc.arg(band)::text = '' OR CASE WHEN k.system_id >= 32000000 THEN 'abyssal'
                WHEN k.system_id >= 31000000 THEN 'wh'
                WHEN k.region_id = 10000070 THEN 'pochven'
                WHEN COALESCE(ss.security, 0) >= 0.45 THEN 'high'
                WHEN COALESCE(ss.security, 0) > 0 THEN 'low'
                ELSE 'null' END = sqlc.arg(band)::text)
  AND (sqlc.arg(side)::integer = 0 OR ((CASE sqlc.arg(kind)::text WHEN 'character' THEN k.victim_character_id
                                                WHEN 'corporation' THEN k.victim_corporation_id
                                                ELSE k.victim_alliance_id END) = sqlc.arg(scope_id)::bigint) = (sqlc.arg(side)::integer = 2))
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: KillScanEdgeEntity :one
SELECT f.killmail_time, f.killmail_id FROM kill_entity_feed f WHERE f.kind = sqlc.arg(kind)::text AND f.entity_id = sqlc.arg(scope_id)::bigint AND (f.killmail_time, f.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY f.killmail_time DESC, f.killmail_id DESC
OFFSET sqlc.arg(scan_cap)::bigint - 1
LIMIT 1;

-- Every system's region, constellation and security, loaded once
-- into memory for the live stream's filters and the sov fight
-- correlation.
-- name: ListKillPlaceIndex :many
SELECT system_id, region_id, constellation_id, security FROM sde_systems;

-- The ship class of every type a killmail can name as its victim:
-- ships, structures, starbases, orbitals and deployables.
-- name: ListKillTypeGroups :many
SELECT t.type_id, t.group_id FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id IN (6, 22, 23, 40, 46, 65);

-- The ship class picker: published groups in those categories.
-- name: ListKillFilterGroups :many
SELECT g.group_id, g.name, g.category_id FROM sde_groups g
WHERE g.category_id IN (6, 22, 23, 40, 46, 65)
  AND EXISTS (SELECT 1 FROM sde_types t WHERE t.group_id = g.group_id AND t.published = 1)
ORDER BY g.name;

-- The region picker: known space and Pochven.
-- name: ListKillFilterRegions :many
SELECT region_id, name FROM sde_regions WHERE region_id < 11000000 ORDER BY name;
