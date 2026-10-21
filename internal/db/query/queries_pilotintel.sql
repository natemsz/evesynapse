-- ---------------------------------------------------------------------
-- Pilot intel (pilot_intel.go): what our killboard, the reader's intel
-- channels and their own scans say about one pilot. Kills count only
-- when public (posted to zKillboard); NPC kills never.
-- ---------------------------------------------------------------------

-- name: PilotKillStats :one
SELECT
    COUNT(DISTINCT a.killmail_id) FILTER (WHERE k.victim_character_id <> sqlc.arg(character_id))::bigint AS kills,
    COUNT(DISTINCT a.killmail_id) FILTER (WHERE k.victim_character_id <> sqlc.arg(character_id) AND k.solo)::bigint AS solo,
    (SELECT COUNT(*) FROM kills v WHERE v.victim_character_id = sqlc.arg(character_id) AND v.public
        AND v.killmail_time >= sqlc.arg(since)::timestamptz)::bigint AS losses,
    COALESCE(SUM(COALESCE(k.value_ours, k.value_zkill, 0)) FILTER (WHERE k.victim_character_id <> sqlc.arg(character_id)), 0)::float8 AS isk_killed
FROM kill_attackers a
JOIN kills k ON k.killmail_id = a.killmail_id
WHERE a.character_id = sqlc.arg(character_id) AND k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz;

-- The pilot's latest public kills and losses, newest first.
-- name: ListPilotRecentKills :many
SELECT k.killmail_id, k.killmail_time, k.system_id, k.ship_type_id, k.victim_character_id,
       COALESCE(k.value_ours, k.value_zkill, 0)::float8 AS value,
       (k.victim_character_id = sqlc.arg(character_id))::boolean AS lost,
       COALESCE((SELECT a.ship_type_id FROM kill_attackers a WHERE a.killmail_id = k.killmail_id
                 AND a.character_id = sqlc.arg(character_id) LIMIT 1), 0)::bigint AS flew
FROM kills k
WHERE k.public AND NOT k.npc AND (k.victim_character_id = sqlc.arg(character_id)
   OR EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id AND a.character_id = sqlc.arg(character_id)))
ORDER BY k.killmail_time DESC
LIMIT sqlc.arg(max_rows)::int;

-- What the pilot flies on kills, most first.
-- name: ListPilotShips :many
SELECT a.ship_type_id, COUNT(DISTINCT a.killmail_id)::bigint AS n
FROM kill_attackers a JOIN kills k ON k.killmail_id = a.killmail_id
WHERE a.character_id = sqlc.arg(character_id) AND k.public AND NOT k.npc AND a.ship_type_id > 0
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY a.ship_type_id ORDER BY n DESC LIMIT 8;

-- Where the pilot kills, most first.
-- name: ListPilotSystems :many
SELECT k.system_id, COUNT(DISTINCT k.killmail_id)::bigint AS n, MAX(k.killmail_time)::timestamptz AS last_at
FROM kill_attackers a JOIN kills k ON k.killmail_id = a.killmail_id
WHERE a.character_id = sqlc.arg(character_id) AND k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY k.system_id ORDER BY n DESC LIMIT 8;

-- The hours (EVE time) the pilot kills in.
-- name: ListPilotHours :many
SELECT EXTRACT(HOUR FROM k.killmail_time AT TIME ZONE 'UTC')::int AS hour, COUNT(DISTINCT k.killmail_id)::bigint AS n
FROM kill_attackers a JOIN kills k ON k.killmail_id = a.killmail_id
WHERE a.character_id = sqlc.arg(character_id) AND k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY 1;

-- Who the pilot flies with: the pilots on the same kills, most first.
-- name: ListPilotCompanions :many
SELECT o.character_id, MAX(o.corporation_id)::bigint AS corporation_id, MAX(o.alliance_id)::bigint AS alliance_id,
       COUNT(DISTINCT o.killmail_id)::bigint AS n
FROM kill_attackers a
JOIN kills k ON k.killmail_id = a.killmail_id
JOIN kill_attackers o ON o.killmail_id = a.killmail_id AND o.character_id <> a.character_id AND o.character_id > 0
WHERE a.character_id = sqlc.arg(character_id) AND k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY o.character_id ORDER BY n DESC LIMIT 10;

-- Intel reports naming the pilot, in channels the reader can read.
-- name: ListReportsNaming :many
SELECT id, channel_id, system_id, reporter, text, hostiles, clear, seen_at
FROM intel_reports
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND seen_at >= sqlc.arg(since)::timestamptz
  AND text ILIKE '%' || sqlc.arg(pattern)::text || '%' ESCAPE '\'
ORDER BY seen_at DESC LIMIT 10;

-- The reader's own local scans with the pilot in them.
-- name: ListScansWithPilot :many
SELECT id, system_id, created_at FROM scans
WHERE kind = 'local'
  AND ((sqlc.arg(user_id)::bigint > 0 AND user_id = sqlc.arg(user_id)::bigint) OR id = ANY(sqlc.arg(ids)::text[]))
  AND result::jsonb -> 'local' -> 'pilots' @> jsonb_build_array(jsonb_build_object('id', sqlc.arg(character_id)::bigint))
ORDER BY created_at DESC LIMIT 10;

-- Known hostiles.
-- name: AddIntelWatch :exec
INSERT INTO intel_watch (channel_id, character_id, note, added_by, added_at)
VALUES (sqlc.arg(channel_id), sqlc.arg(character_id), sqlc.arg(note), sqlc.arg(added_by), sqlc.arg(added_at))
ON CONFLICT (channel_id, character_id) DO UPDATE SET note = EXCLUDED.note;

-- name: RemoveIntelWatch :exec
DELETE FROM intel_watch WHERE channel_id = sqlc.arg(channel_id) AND character_id = sqlc.arg(character_id);

-- name: ListIntelWatch :many
SELECT channel_id, character_id, note, added_at FROM intel_watch
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
ORDER BY added_at DESC;

-- Where each of some pilots was last seen on a public kill, since a
-- moment: the map's "Watched pilots".
-- name: ListPilotsLastSeen :many
SELECT DISTINCT ON (p.character_id) p.character_id, k.system_id, k.killmail_time
FROM (
    SELECT a.character_id, a.killmail_id FROM kill_attackers a WHERE a.character_id = ANY(sqlc.arg(character_ids)::bigint[])
    UNION ALL
    SELECT v.victim_character_id, v.killmail_id FROM kills v WHERE v.victim_character_id = ANY(sqlc.arg(character_ids)::bigint[])
) p
JOIN kills k ON k.killmail_id = p.killmail_id
WHERE k.public AND k.killmail_time >= sqlc.arg(since)::timestamptz
ORDER BY p.character_id, k.killmail_time DESC;
