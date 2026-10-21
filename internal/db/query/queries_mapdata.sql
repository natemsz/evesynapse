-- ---------------------------------------------------------------------
-- The map's data layers (map_mine.go): an account's own data, by
-- system. Reads only.
-- ---------------------------------------------------------------------

-- The system each of some locations is in: NPC stations from the SDE,
-- player structures from what structure resolution learned.
-- name: ListLocationSystems :many
SELECT station_id::bigint AS location_id, system_id::bigint AS system_id
FROM sde_stations WHERE station_id = ANY(sqlc.arg(ids)::bigint[]) AND system_id > 0
UNION ALL
SELECT structure_id::bigint, system_id::bigint
FROM structure_context WHERE structure_id = ANY(sqlc.arg(ids)::bigint[]) AND system_id > 0;

-- An account's characters' kills and losses since a moment, by system:
-- kills they were on (once a kill however many of them were), losses
-- they took, and the ISK of each by our appraisal (zKillboard's when
-- ours is not in yet). Their own private kills count: these are theirs.
-- name: ListAccountKillsBySystem :many
WITH mine AS (
    SELECT DISTINCT k.killmail_id, k.system_id, COALESCE(k.value_ours, k.value_zkill, 0)::float8 AS isk,
           (k.victim_character_id = ANY(sqlc.arg(character_ids)::bigint[])) AS lost
    FROM kills k
    WHERE k.killmail_time >= sqlc.arg(since)::timestamptz
      AND (k.victim_character_id = ANY(sqlc.arg(character_ids)::bigint[])
           OR EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id
                      AND a.character_id = ANY(sqlc.arg(character_ids)::bigint[])))
)
SELECT system_id,
       COUNT(*) FILTER (WHERE NOT lost)::bigint AS kills,
       COUNT(*) FILTER (WHERE lost)::bigint AS losses,
       COALESCE(SUM(isk) FILTER (WHERE NOT lost), 0)::float8 AS isk_killed,
       COALESCE(SUM(isk) FILTER (WHERE lost), 0)::float8 AS isk_lost
FROM mine
GROUP BY system_id;

-- The systems an account posted intel reports about since a moment.
-- name: ListAuthoredIntelBySystem :many
SELECT system_id, COUNT(*)::bigint AS reports, MAX(seen_at)::timestamptz AS last_seen
FROM intel_reports
WHERE author_user_id = sqlc.arg(user_id) AND seen_at >= sqlc.arg(since)::timestamptz
GROUP BY system_id;

-- An account's scans that name a system, newest first.
-- name: ListAccountScanSystems :many
SELECT id, kind, system_id, created_at FROM scans
WHERE user_id = sqlc.arg(user_id) AND system_id > 0
ORDER BY created_at DESC
LIMIT 200;
