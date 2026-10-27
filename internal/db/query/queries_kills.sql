-- ---------------------------------------------------------------------
-- The killboard (schema 074): the stream follower's cursor, kill
-- ingest from either source, the pricing pass, retention.
-- ---------------------------------------------------------------------

-- name: GetKillfeedState :one
SELECT sequence, updated_at FROM killfeed_state WHERE id = 1;

-- name: SetKillfeedState :exec
INSERT INTO killfeed_state (id, sequence, updated_at)
VALUES (1, sqlc.arg(sequence)::bigint, sqlc.arg(updated_at)::timestamptz)
ON CONFLICT (id) DO UPDATE SET sequence = EXCLUDED.sequence, updated_at = EXCLUDED.updated_at;

-- One kill, from either source. A kill already stored keeps its row:
-- the stream's arrival makes a private one public and brings
-- zKillboard's figures; our characters' ESI copy never makes a public
-- one private. inserted reports whether this was the first copy.
-- name: UpsertKill :one
INSERT INTO kills (killmail_id, hash, killmail_time, system_id, region_id, location_id,
    victim_character_id, victim_corporation_id, victim_alliance_id, victim_faction_id,
    ship_type_id, damage_taken, attacker_count,
    fb_character_id, fb_corporation_id, fb_alliance_id, fb_ship_type_id,
    npc, solo, awox, points, value_zkill, source, public, ingested_at)
VALUES (sqlc.arg(killmail_id), sqlc.arg(hash), sqlc.arg(killmail_time), sqlc.arg(system_id),
    sqlc.arg(region_id), sqlc.arg(location_id),
    sqlc.arg(victim_character_id), sqlc.arg(victim_corporation_id), sqlc.arg(victim_alliance_id),
    sqlc.arg(victim_faction_id), sqlc.arg(ship_type_id), sqlc.arg(damage_taken), sqlc.arg(attacker_count),
    sqlc.arg(fb_character_id), sqlc.arg(fb_corporation_id), sqlc.arg(fb_alliance_id), sqlc.arg(fb_ship_type_id),
    sqlc.arg(npc), sqlc.arg(solo), sqlc.arg(awox), sqlc.arg(points),
    sqlc.narg(value_zkill), sqlc.arg(source), sqlc.arg(public), sqlc.arg(ingested_at))
ON CONFLICT (killmail_id) DO UPDATE SET
    public      = kills.public OR EXCLUDED.public,
    source      = CASE WHEN EXCLUDED.public AND NOT kills.public THEN EXCLUDED.source ELSE kills.source END,
    location_id = GREATEST(kills.location_id, EXCLUDED.location_id),
    npc         = CASE WHEN EXCLUDED.source = 'zkill' THEN EXCLUDED.npc ELSE kills.npc END,
    solo        = CASE WHEN EXCLUDED.source = 'zkill' THEN EXCLUDED.solo ELSE kills.solo END,
    awox        = CASE WHEN EXCLUDED.source = 'zkill' THEN EXCLUDED.awox ELSE kills.awox END,
    points      = GREATEST(kills.points, EXCLUDED.points),
    value_zkill = COALESCE(EXCLUDED.value_zkill, kills.value_zkill)
RETURNING (xmax = 0) AS inserted;

-- name: InsertKillAttackers :exec
INSERT INTO kill_attackers (killmail_id, seq, character_id, corporation_id, alliance_id,
    faction_id, ship_type_id, weapon_type_id, damage_done, final_blow)
SELECT sqlc.arg(killmail_id)::bigint,
    unnest(sqlc.arg(seqs)::int[]), unnest(sqlc.arg(character_ids)::bigint[]),
    unnest(sqlc.arg(corporation_ids)::bigint[]), unnest(sqlc.arg(alliance_ids)::bigint[]),
    unnest(sqlc.arg(faction_ids)::bigint[]), unnest(sqlc.arg(ship_type_ids)::bigint[]),
    unnest(sqlc.arg(weapon_type_ids)::bigint[]), unnest(sqlc.arg(damage_dones)::bigint[]),
    unnest(sqlc.arg(final_blows)::boolean[])
ON CONFLICT DO NOTHING;

-- name: InsertKillDetail :exec
INSERT INTO kill_details (killmail_id, payload, stored_at)
VALUES (sqlc.arg(killmail_id), sqlc.arg(payload), sqlc.arg(stored_at))
ON CONFLICT (killmail_id) DO NOTHING;

-- Our characters' ESI killmails the board has not stored yet.
-- name: ListESIKillsToMerge :many
SELECT DISTINCT ON (d.killmail_id) d.killmail_id, d.hash, d.payload
FROM killmail_details d
WHERE NOT EXISTS (SELECT 1 FROM kills k WHERE k.killmail_id = d.killmail_id)
ORDER BY d.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- The pricing pass's slice: kills not priced yet whose full
-- killmail is still held, newest first.
-- name: ListUnpricedKills :many
SELECT k.killmail_id, k.ship_type_id, d.payload
FROM kills k
JOIN kill_details d ON d.killmail_id = k.killmail_id
WHERE k.priced_at IS NULL
ORDER BY k.killmail_time DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: InsertKillLinePrices :exec
INSERT INTO kill_line_prices (killmail_id, type_id, unit_price)
SELECT sqlc.arg(killmail_id)::bigint,
    unnest(sqlc.arg(type_ids)::bigint[]), unnest(sqlc.arg(unit_prices)::float8[])
ON CONFLICT (killmail_id, type_id) DO NOTHING;

-- name: GetKillLinePrices :many
SELECT type_id, unit_price FROM kill_line_prices
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- name: SetKillValue :exec
UPDATE kills SET value_ours = sqlc.arg(value_ours)::double precision,
    value_dropped_ours = sqlc.arg(value_dropped_ours)::double precision,
    value_coverage = sqlc.arg(value_coverage)::real,
    priced_at = sqlc.arg(priced_at)::timestamptz
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- Retention: attackers and full killmails older than the cutoff go;
-- the summary row stays.
-- name: PruneKillDetails :execrows
DELETE FROM kill_details d USING kills k
WHERE k.killmail_id = d.killmail_id AND k.killmail_time < sqlc.arg(before)::timestamptz;

-- name: PruneKillAttackers :execrows
DELETE FROM kill_attackers a USING kills k
WHERE k.killmail_id = a.killmail_id AND k.killmail_time < sqlc.arg(before)::timestamptz;

-- name: ListSystemRegions :many
SELECT system_id, region_id FROM sde_systems;

-- name: CountKillsSince :one
SELECT COUNT(*) FROM kills WHERE ingested_at >= sqlc.arg(since)::timestamptz;

-- ---------------------------------------------------------------------
-- The killboard pages: public kills only, newest first, one query per
-- scope so each rides its own index. Paging is keyset: the page after
-- a kill is everything strictly older (time, then id). min_value 0
-- shows everything; solo_only narrows to lone-attacker kills.
-- ---------------------------------------------------------------------

-- name: ListKillsRecent :many
SELECT * FROM kills k
WHERE k.public
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillsBySystem :many
SELECT * FROM kills k
WHERE k.public AND k.system_id = sqlc.arg(system_id)::bigint
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillsByRegion :many
SELECT * FROM kills k
WHERE k.public AND k.region_id = sqlc.arg(region_id)::bigint
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillsByShip :many
SELECT * FROM kills k
WHERE k.public AND k.ship_type_id = sqlc.arg(ship_type_id)::bigint
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- An entity's kills and losses: the victim side from kills, the
-- attacker side from kill_attackers (a year deep).
-- name: ListKillsByCharacter :many
SELECT * FROM kills k
WHERE k.public
  AND (k.victim_character_id = sqlc.arg(entity_id)::bigint
       OR k.killmail_id IN (SELECT a.killmail_id FROM kill_attackers a WHERE a.character_id = sqlc.arg(entity_id)::bigint))
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillsByCorporation :many
SELECT * FROM kills k
WHERE k.public
  AND (k.victim_corporation_id = sqlc.arg(entity_id)::bigint
       OR k.killmail_id IN (SELECT a.killmail_id FROM kill_attackers a WHERE a.corporation_id = sqlc.arg(entity_id)::bigint))
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillsByAlliance :many
SELECT * FROM kills k
WHERE k.public
  AND (k.victim_alliance_id = sqlc.arg(entity_id)::bigint
       OR k.killmail_id IN (SELECT a.killmail_id FROM kill_attackers a WHERE a.alliance_id = sqlc.arg(entity_id)::bigint))
  AND (k.killmail_time, k.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY k.killmail_time DESC, k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: GetPublicKill :one
SELECT * FROM kills WHERE killmail_id = $1 AND public;

-- name: GetKillDetail :one
SELECT payload FROM kill_details WHERE killmail_id = $1;

-- The board's headline: every kill counts (private ones too: these
-- are totals, never a kill shown).
-- name: GetKillStats :one
SELECT
    COUNT(*) FILTER (WHERE killmail_time >= sqlc.arg(hour_ago)::timestamptz)::bigint AS kills_hour,
    COUNT(*)::bigint AS kills_day,
    COALESCE(SUM(value_ours), 0)::double precision AS isk_day
FROM kills
WHERE killmail_time >= sqlc.arg(day_ago)::timestamptz AND NOT npc;

-- name: GetBiggestKill :one
SELECT * FROM kills
WHERE public AND killmail_time >= sqlc.arg(since)::timestamptz AND value_ours IS NOT NULL
ORDER BY value_ours DESC
LIMIT 1;

-- Leaderboards over a window, public kills, NPC deaths left out.
-- name: TopKillerAlliances :many
SELECT p.alliance_id, COUNT(*)::bigint AS kills, COALESCE(SUM(p.value_ours), 0)::double precision AS isk
FROM (
    SELECT DISTINCT a.alliance_id, k.killmail_id, k.value_ours
    FROM kill_attackers a
    JOIN kills k ON k.killmail_id = a.killmail_id
    WHERE k.public AND NOT k.npc AND a.alliance_id > 0 AND k.killmail_time >= sqlc.arg(since)::timestamptz
) p
GROUP BY p.alliance_id
ORDER BY kills DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: TopShipsLost :many
SELECT ship_type_id, COUNT(*)::bigint AS losses
FROM kills
WHERE public AND NOT npc AND killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY ship_type_id
ORDER BY losses DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: TopKillSystems :many
SELECT system_id, COUNT(*)::bigint AS kills
FROM kills
WHERE public AND NOT npc AND killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY system_id
ORDER BY kills DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- Names the board will show: recent public kills' people, corps and
-- alliances, victim and attacker side.
-- name: ListRecentKillEntities :many
SELECT DISTINCT unnest(ARRAY[k.victim_character_id, k.victim_corporation_id, k.victim_alliance_id,
    a.character_id, a.corporation_id, a.alliance_id])::bigint AS id
FROM kills k
LEFT JOIN kill_attackers a ON a.killmail_id = k.killmail_id
WHERE k.public AND k.ingested_at >= sqlc.arg(since)::timestamptz;

-- ---------------------------------------------------------------------
-- The map's threat layer (map_threat.go): recent PvP kills per system
-- (every kill counts toward the score, private ones too -- it is a
-- total), and the attackers of recent public kills (a camp is named
-- from public kills only).
-- ---------------------------------------------------------------------

-- name: ListThreatKills :many
SELECT killmail_id, system_id, killmail_time, public, victim_corporation_id, ship_type_id, location_id
FROM kills
WHERE killmail_time >= sqlc.arg(since)::timestamptz AND NOT npc
ORDER BY killmail_time;

-- name: ListThreatAttackers :many
SELECT a.killmail_id, a.character_id, a.corporation_id, a.alliance_id
FROM kill_attackers a
JOIN kills k ON k.killmail_id = a.killmail_id
WHERE k.killmail_time >= sqlc.arg(since)::timestamptz AND NOT k.npc AND k.public AND a.character_id > 0;

-- name: GetLatestKillIngest :one
SELECT COALESCE(MAX(ingested_at), 'epoch'::timestamptz)::timestamptz AS latest FROM kills;

-- name: ListSystemThreatKills :many
SELECT killmail_id, system_id, killmail_time, public, victim_corporation_id, ship_type_id, location_id
FROM kills
WHERE system_id = sqlc.arg(system_id)::bigint AND killmail_time >= sqlc.arg(since)::timestamptz AND NOT npc
ORDER BY killmail_time;

-- name: ListSystemKillAttackers :many
SELECT a.killmail_id, a.character_id, a.corporation_id, a.alliance_id, a.ship_type_id
FROM kill_attackers a
JOIN kills k ON k.killmail_id = a.killmail_id
WHERE k.system_id = sqlc.arg(system_id)::bigint AND k.killmail_time >= sqlc.arg(since)::timestamptz
  AND NOT k.npc AND k.public AND a.character_id > 0;
