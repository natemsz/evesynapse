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

-- Pilot activity profiles (schema 087, pilot_activity.go): the
-- stored aggregates the pilot page's kill history reads. Written
-- at ingest only: the one-time history backfill of a pilot met in
-- a reading, and the live kill stream for pilots who have one.

-- name: GetPilotActivityProfile :one
SELECT character_id, backfilled_at, updated_at, kills, losses, final_blows,
       gang_total, gang_samples, usual_ship_type_id, last_active_at
FROM pilot_activity_profiles
WHERE character_id = sqlc.arg(character_id);

-- The backfill claims a pilot before fetching: the row's presence
-- is what lets the live stream maintain the profile meanwhile.
-- name: ClaimPilotActivityProfile :exec
INSERT INTO pilot_activity_profiles (character_id, updated_at)
VALUES (sqlc.arg(character_id), sqlc.arg(updated_at))
ON CONFLICT (character_id) DO NOTHING;

-- name: MarkPilotActivityBackfilled :exec
UPDATE pilot_activity_profiles
SET backfilled_at = sqlc.arg(backfilled_at), updated_at = sqlc.arg(backfilled_at)
WHERE character_id = sqlc.arg(character_id);

-- One marker per (kill, pilot) folded into the aggregates. A
-- conflict (no row back) means the kill already counted.
-- name: MarkPilotKillApplied :one
INSERT INTO pilot_activity_applied (killmail_id, character_id, killmail_time)
VALUES (sqlc.arg(killmail_id), sqlc.arg(character_id), sqlc.arg(killmail_time))
ON CONFLICT DO NOTHING
RETURNING killmail_id;

-- name: ApplyPilotActivityKill :exec
INSERT INTO pilot_activity_profiles (character_id, updated_at, kills, losses, final_blows,
       gang_total, gang_samples, last_active_at)
VALUES (sqlc.arg(character_id), sqlc.arg(applied_at), sqlc.arg(kills), sqlc.arg(losses),
       sqlc.arg(final_blows), sqlc.arg(gang_total), sqlc.arg(gang_samples), sqlc.arg(killmail_time))
ON CONFLICT (character_id) DO UPDATE SET
    kills = pilot_activity_profiles.kills + EXCLUDED.kills,
    losses = pilot_activity_profiles.losses + EXCLUDED.losses,
    final_blows = pilot_activity_profiles.final_blows + EXCLUDED.final_blows,
    gang_total = pilot_activity_profiles.gang_total + EXCLUDED.gang_total,
    gang_samples = pilot_activity_profiles.gang_samples + EXCLUDED.gang_samples,
    last_active_at = GREATEST(pilot_activity_profiles.last_active_at, EXCLUDED.last_active_at),
    updated_at = EXCLUDED.updated_at;

-- name: ApplyPilotActivityShip :exec
INSERT INTO pilot_activity_ships (character_id, ship_type_id, flown, final_blows, lost)
VALUES (sqlc.arg(character_id), sqlc.arg(ship_type_id), sqlc.arg(flown), sqlc.arg(final_blows), sqlc.arg(lost))
ON CONFLICT (character_id, ship_type_id) DO UPDATE SET
    flown = pilot_activity_ships.flown + EXCLUDED.flown,
    final_blows = pilot_activity_ships.final_blows + EXCLUDED.final_blows,
    lost = pilot_activity_ships.lost + EXCLUDED.lost;

-- name: ApplyPilotActivitySystem :exec
INSERT INTO pilot_activity_systems (character_id, system_id, events, last_at)
VALUES (sqlc.arg(character_id), sqlc.arg(system_id), 1, sqlc.arg(killmail_time))
ON CONFLICT (character_id, system_id) DO UPDATE SET
    events = pilot_activity_systems.events + 1,
    last_at = GREATEST(pilot_activity_systems.last_at, EXCLUDED.last_at);

-- name: ApplyPilotActivityHour :exec
INSERT INTO pilot_activity_hours (character_id, hour, events)
VALUES (sqlc.arg(character_id), sqlc.arg(hour), 1)
ON CONFLICT (character_id, hour) DO UPDATE SET
    events = pilot_activity_hours.events + 1;

-- The usual hull: the ship with the most final blows, kept on the
-- profile row so the page reads it without a sort.
-- name: RefreshPilotUsualShip :exec
UPDATE pilot_activity_profiles p
SET usual_ship_type_id = COALESCE((
    SELECT s.ship_type_id FROM pilot_activity_ships s
    WHERE s.character_id = p.character_id AND s.final_blows > 0
    ORDER BY s.final_blows DESC, s.flown DESC, s.ship_type_id
    LIMIT 1), 0)
WHERE p.character_id = sqlc.arg(character_id);

-- name: ListPilotActivityShips :many
SELECT ship_type_id, flown, final_blows, lost
FROM pilot_activity_ships
WHERE character_id = sqlc.arg(character_id) AND (flown + lost) > 0
ORDER BY (flown + lost) DESC, ship_type_id
LIMIT sqlc.arg(max_rows)::int;

-- name: ListPilotActivitySystems :many
SELECT system_id, events, last_at
FROM pilot_activity_systems
WHERE character_id = sqlc.arg(character_id)
ORDER BY events DESC, system_id
LIMIT sqlc.arg(max_rows)::int;

-- name: ListPilotActivityHours :many
SELECT hour, events
FROM pilot_activity_hours
WHERE character_id = sqlc.arg(character_id);

-- Markers older than the kill details' retention go with them.
-- name: PrunePilotActivityApplied :execrows
DELETE FROM pilot_activity_applied
WHERE killmail_time < sqlc.arg(before)::timestamptz;

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
