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
