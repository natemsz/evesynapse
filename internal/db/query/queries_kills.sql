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

-- The kill's permanent involvement record: one slim row per
-- attacker, identity columns only. Written on the first copy
-- whether or not the attacker detail is stored with it, so a
-- kill stored in post-prune shape still credits its value.
-- name: InsertKillInvolvement :exec
INSERT INTO kill_involvement (killmail_id, character_id, corporation_id, alliance_id)
SELECT sqlc.arg(killmail_id)::bigint,
    unnest(sqlc.arg(character_ids)::bigint[]),
    unnest(sqlc.arg(corporation_ids)::bigint[]),
    unnest(sqlc.arg(alliance_ids)::bigint[])
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
INSERT INTO kill_line_prices (killmail_id, type_id, unit_price, tier)
SELECT sqlc.arg(killmail_id)::bigint,
    unnest(sqlc.arg(type_ids)::bigint[]), unnest(sqlc.arg(unit_prices)::float8[]),
    unnest(sqlc.arg(tiers)::text[])
ON CONFLICT (killmail_id, type_id) DO NOTHING;

-- name: GetKillLinePrices :many
SELECT type_id, unit_price, tier FROM kill_line_prices
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- name: SetKillValue :exec
UPDATE kills SET value_ours = sqlc.arg(value_ours)::double precision,
    value_dropped_ours = sqlc.arg(value_dropped_ours)::double precision,
    value_coverage = sqlc.arg(value_coverage)::real,
    priced_at = sqlc.arg(priced_at)::timestamptz
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- Kills inside retention whose attacker rows are gone (pruned before
-- the stats fold counted their ships) and that the archive restore
-- could not serve, newest first below a keyset cursor, for the
-- restorer to read back from ESI by hash.
-- name: ListKillsMissingAttackers :many
SELECT k.killmail_id, k.hash, k.killmail_time FROM kills k
WHERE k.killmail_id < sqlc.arg(before_id)::bigint
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
  AND k.attacker_count > 0 AND k.hash <> ''
  AND NOT EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id)
ORDER BY k.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- Retention: attacker detail, full killmails and per-item priced
-- lines older than the cutoff go; the summary row and the
-- involvement record stay. Values and who they credit are
-- permanent; only detail ages out. Past two years a kill is metadata
-- for intel only (who, when, ship, total value), not a record.
-- Raw kill rows past retention go only once the kill is counted
-- into the daily statistics (it is not waiting in
-- kill_stats_pending): the fold reads the attacker rows for ships
-- flown, so pruning first would lose them from the stats for good.
-- name: PruneKillDetails :execrows
DELETE FROM kill_details d USING kills k
WHERE k.killmail_id = d.killmail_id AND k.killmail_time < sqlc.arg(before)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = k.killmail_id);

-- name: PruneKillAttackers :execrows
DELETE FROM kill_attackers a USING kills k
WHERE k.killmail_id = a.killmail_id AND k.killmail_time < sqlc.arg(before)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = k.killmail_id);

-- name: PruneKillLinePrices :execrows
DELETE FROM kill_line_prices l USING kills k
WHERE k.killmail_id = l.killmail_id AND k.killmail_time < sqlc.arg(before)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = k.killmail_id);

-- name: ListSystemRegions :many
SELECT system_id, region_id FROM sde_systems;

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

-- name: GetPublicKill :one
SELECT * FROM kills WHERE killmail_id = $1 AND public;

-- One kill whether or not it is public: our own characters' kills
-- and losses, for pages that already know the viewer owns it.
-- name: GetKill :one
SELECT * FROM kills WHERE killmail_id = $1;

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
-- TopKillerAlliances starts from the window's kills (the time
-- index) and looks up each kill's attacking alliances by killmail
-- id, so a run reads one day of rows, never the whole alliance
-- index.
-- name: TopKillerAlliances :many
WITH recent AS MATERIALIZED (
    SELECT k.killmail_id, k.value_ours FROM kills k
    WHERE k.public AND NOT k.npc AND k.killmail_time >= sqlc.arg(since)::timestamptz
)
SELECT a.alliance_id, COUNT(*)::bigint AS kills, COALESCE(SUM(r.value_ours), 0)::double precision AS isk
FROM recent r
CROSS JOIN LATERAL (
    SELECT DISTINCT x.alliance_id FROM kill_attackers x
    WHERE x.killmail_id = r.killmail_id AND x.alliance_id > 0
) a
GROUP BY a.alliance_id
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

-- ---------------------------------------------------------------------
-- Corporation killboard coverage (schema 126, corp_backfill.go):
-- the marker the corporation history backfill checks before
-- fetching and sets when a pass completes, the pilot activity
-- profile's BackfilledAt discipline without the aggregates
-- corporations do not keep.
-- ---------------------------------------------------------------------

-- name: GetCorpKillCoverage :one
SELECT corporation_id, backfilled_at FROM corp_kill_coverage
WHERE corporation_id = sqlc.arg(corporation_id)::bigint;

-- name: MarkCorpKillCoverage :exec
INSERT INTO corp_kill_coverage (corporation_id, backfilled_at)
VALUES (sqlc.arg(corporation_id)::bigint, sqlc.arg(backfilled_at)::timestamptz)
ON CONFLICT (corporation_id) DO NOTHING;

-- ---------------------------------------------------------------------
-- Killboard statistics (schema 131, killboard_stats.go): the fold
-- at ingest and the stored reads the pages use. Nothing here
-- aggregates the kills table; the daily buckets are the largest
-- thing a render sums, a few hundred rows for all time.
-- ---------------------------------------------------------------------

-- name: ListKillInvolvement :many
SELECT character_id, corporation_id, alliance_id FROM kill_involvement
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- name: ListKillAttackerShips :many
SELECT character_id, corporation_id, alliance_id, ship_type_id FROM kill_attackers
WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- name: ApplyKillEntityDaily :exec
INSERT INTO kill_entity_daily (day, kind, entity_id, kills, solo, losses, isk_killed, isk_lost, biggest_killmail_id, biggest_value)
VALUES (sqlc.arg(day)::date, sqlc.arg(kind)::text, sqlc.arg(entity_id)::bigint,
    sqlc.arg(kills)::bigint, sqlc.arg(solo)::bigint, sqlc.arg(losses)::bigint,
    sqlc.arg(isk_killed)::double precision, sqlc.arg(isk_lost)::double precision,
    sqlc.arg(biggest_killmail_id)::bigint, sqlc.arg(biggest_value)::double precision)
ON CONFLICT (day, kind, entity_id) DO UPDATE SET
    kills = kill_entity_daily.kills + EXCLUDED.kills,
    solo = kill_entity_daily.solo + EXCLUDED.solo,
    losses = kill_entity_daily.losses + EXCLUDED.losses,
    isk_killed = kill_entity_daily.isk_killed + EXCLUDED.isk_killed,
    isk_lost = kill_entity_daily.isk_lost + EXCLUDED.isk_lost,
    biggest_killmail_id = CASE WHEN EXCLUDED.biggest_value > kill_entity_daily.biggest_value THEN EXCLUDED.biggest_killmail_id ELSE kill_entity_daily.biggest_killmail_id END,
    biggest_value = GREATEST(kill_entity_daily.biggest_value, EXCLUDED.biggest_value);

-- name: ApplyKillEntityDailyShip :exec
INSERT INTO kill_entity_daily_ships (day, kind, entity_id, ship_type_id, flown, lost)
VALUES (sqlc.arg(day)::date, sqlc.arg(kind)::text, sqlc.arg(entity_id)::bigint, sqlc.arg(ship_type_id)::bigint,
    sqlc.arg(flown)::bigint, sqlc.arg(lost)::bigint)
ON CONFLICT (day, kind, entity_id, ship_type_id) DO UPDATE SET
    flown = kill_entity_daily_ships.flown + EXCLUDED.flown,
    lost = kill_entity_daily_ships.lost + EXCLUDED.lost;

-- name: ApplyKillEntityDailySystem :exec
INSERT INTO kill_entity_daily_systems (day, kind, entity_id, system_id, events)
VALUES (sqlc.arg(day)::date, sqlc.arg(kind)::text, sqlc.arg(entity_id)::bigint, sqlc.arg(system_id)::bigint, 1)
ON CONFLICT (day, kind, entity_id, system_id) DO UPDATE SET
    events = kill_entity_daily_systems.events + 1;

-- name: GetKillEntityStats :one
SELECT COALESCE(SUM(kills), 0)::bigint AS kills, COALESCE(SUM(solo), 0)::bigint AS solo,
    COALESCE(SUM(losses), 0)::bigint AS losses,
    COALESCE(SUM(isk_killed), 0)::double precision AS isk_killed,
    COALESCE(SUM(isk_lost), 0)::double precision AS isk_lost
FROM kill_entity_daily
WHERE kind = sqlc.arg(kind)::text AND entity_id = sqlc.arg(entity_id)::bigint
  AND day >= (sqlc.arg(since)::timestamptz)::date;

-- name: GetKillEntityBiggest :one
SELECT biggest_killmail_id FROM kill_entity_daily
WHERE kind = sqlc.arg(kind)::text AND entity_id = sqlc.arg(entity_id)::bigint
  AND day >= (sqlc.arg(since)::timestamptz)::date AND biggest_killmail_id > 0
ORDER BY biggest_value DESC
LIMIT 1;

-- name: ListKillEntityTopShips :many
SELECT ship_type_id, COALESCE(SUM(flown), 0)::bigint AS flown, COALESCE(SUM(lost), 0)::bigint AS lost
FROM kill_entity_daily_ships
WHERE kind = sqlc.arg(kind)::text AND entity_id = sqlc.arg(entity_id)::bigint
  AND day >= (sqlc.arg(since)::timestamptz)::date
GROUP BY ship_type_id
ORDER BY flown DESC, lost DESC, ship_type_id
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillEntityShipsLost :many
SELECT ship_type_id, COALESCE(SUM(lost), 0)::bigint AS lost
FROM kill_entity_daily_ships
WHERE kind = sqlc.arg(kind)::text AND entity_id = sqlc.arg(entity_id)::bigint
  AND day >= (sqlc.arg(since)::timestamptz)::date AND lost > 0
GROUP BY ship_type_id
ORDER BY lost DESC, ship_type_id
LIMIT sqlc.arg(max_rows)::bigint;

-- name: ListKillEntityTopSystems :many
SELECT system_id, COALESCE(SUM(events), 0)::bigint AS n
FROM kill_entity_daily_systems
WHERE kind = sqlc.arg(kind)::text AND entity_id = sqlc.arg(entity_id)::bigint
  AND day >= (sqlc.arg(since)::timestamptz)::date
GROUP BY system_id
ORDER BY n DESC, system_id
LIMIT sqlc.arg(max_rows)::bigint;

-- The counting queue (schema 146): public kills not yet folded into
-- the daily aggregates. The fold claims a kill by deleting its row in
-- the aggregates' transaction; no row, no count.
-- name: GetKillPublicForUpdate :one
SELECT public FROM kills WHERE killmail_id = sqlc.arg(killmail_id)::bigint FOR UPDATE;

-- name: InsertKillStatsPending :exec
INSERT INTO kill_stats_pending (killmail_id, killmail_time)
VALUES (sqlc.arg(killmail_id)::bigint, sqlc.arg(killmail_time)::timestamptz)
ON CONFLICT DO NOTHING;

-- name: KillStatsQueued :one
SELECT EXISTS (SELECT 1 FROM kill_stats_pending WHERE killmail_id = sqlc.arg(killmail_id)::bigint) AS queued;

-- name: ClaimKillStatsPending :execrows
DELETE FROM kill_stats_pending WHERE killmail_id = sqlc.arg(killmail_id)::bigint;

-- name: ListKillStatsPending :many
SELECT killmail_id FROM kill_stats_pending
ORDER BY killmail_time DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- The one time rebuild (killboard_rebuild.go): the daily aggregates
-- counted again from complete kill rows, a chunk of killmail ids at
-- a time, into the _rebuild tables. Each chunk applies exactly the
-- fold's rules (foldKillStats): public kills only; attackers from
-- the involvement record, minus the victim's own character,
-- corporation or alliance, and none on an NPC kill; ships flown
-- from the attacker rows; value_ours, else zKillboard's value. A
-- kill waiting in kill_stats_pending is left for the live fold.

-- name: KillStatsRebuildChunkEnd :one
SELECT COALESCE(MAX(c.killmail_id), 0)::bigint AS through_id FROM (
    SELECT killmail_id FROM kills
    WHERE killmail_id > sqlc.arg(after_id)::bigint
    ORDER BY killmail_id
    LIMIT sqlc.arg(max_rows)::bigint
) c;

-- name: RebuildKillEntityDailyChunk :exec
WITH k AS MATERIALIZED (
    SELECT kl.killmail_id, (kl.killmail_time AT TIME ZONE 'UTC')::date AS day, kl.npc, kl.solo,
        kl.victim_character_id AS vc, kl.victim_corporation_id AS vco, kl.victim_alliance_id AS val,
        COALESCE(kl.value_ours, kl.value_zkill, 0)::double precision AS v
    FROM kills kl
    WHERE kl.public AND kl.killmail_id > sqlc.arg(after_id)::bigint AND kl.killmail_id <= sqlc.arg(through_id)::bigint
      AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = kl.killmail_id)
), src AS MATERIALIZED (
    SELECT k.killmail_id, k.vc, k.vco, k.val, t.character_id, t.corporation_id, t.alliance_id
    FROM k CROSS JOIN LATERAL (
        SELECT i.character_id, i.corporation_id, i.alliance_id FROM kill_involvement i WHERE i.killmail_id = k.killmail_id OFFSET 0
    ) t
    WHERE NOT k.npc
), att AS (
    SELECT x.killmail_id, 'character'::text AS kind, x.character_id AS id FROM src x
    WHERE x.character_id > 0 AND x.character_id <> x.vc
    UNION
    SELECT x.killmail_id, 'corporation'::text AS kind, x.corporation_id AS id FROM src x
    WHERE x.corporation_id > 0 AND x.corporation_id <> x.vco
    UNION
    SELECT x.killmail_id, 'alliance'::text AS kind, x.alliance_id AS id FROM src x
    WHERE x.alliance_id > 0 AND x.alliance_id <> x.val
), r AS (
    SELECT k.day, a.kind, a.id AS entity_id, 1::bigint AS kills,
        (CASE WHEN k.solo THEN 1 ELSE 0 END)::bigint AS solo, 0::bigint AS losses,
        k.v AS isk_killed, 0::double precision AS isk_lost, k.killmail_id AS bid, k.v AS bval
    FROM att a JOIN k ON k.killmail_id = a.killmail_id
    UNION ALL
    SELECT k.day, 'character'::text, k.vc, 0::bigint, 0::bigint, 1::bigint, 0::double precision, k.v, 0::bigint, 0::double precision
    FROM k
    WHERE k.vc > 0
    UNION ALL
    SELECT k.day, 'corporation'::text, k.vco, 0::bigint, 0::bigint, 1::bigint, 0::double precision, k.v, 0::bigint, 0::double precision
    FROM k
    WHERE k.vco > 0
    UNION ALL
    SELECT k.day, 'alliance'::text, k.val, 0::bigint, 0::bigint, 1::bigint, 0::double precision, k.v, 0::bigint, 0::double precision
    FROM k
    WHERE k.val > 0
)
INSERT INTO kill_entity_daily_rebuild (day, kind, entity_id, kills, solo, losses, isk_killed, isk_lost, biggest_killmail_id, biggest_value)
SELECT r.day, r.kind, r.entity_id, SUM(r.kills)::bigint, SUM(r.solo)::bigint, SUM(r.losses)::bigint,
    SUM(r.isk_killed)::double precision, SUM(r.isk_lost)::double precision,
    COALESCE((ARRAY_AGG(r.bid ORDER BY r.bval DESC) FILTER (WHERE r.kills = 1))[1], 0)::bigint,
    COALESCE(MAX(r.bval) FILTER (WHERE r.kills = 1), 0)::double precision
FROM r
GROUP BY r.day, r.kind, r.entity_id
ON CONFLICT (day, kind, entity_id) DO UPDATE SET
    kills = kill_entity_daily_rebuild.kills + EXCLUDED.kills,
    solo = kill_entity_daily_rebuild.solo + EXCLUDED.solo,
    losses = kill_entity_daily_rebuild.losses + EXCLUDED.losses,
    isk_killed = kill_entity_daily_rebuild.isk_killed + EXCLUDED.isk_killed,
    isk_lost = kill_entity_daily_rebuild.isk_lost + EXCLUDED.isk_lost,
    biggest_killmail_id = CASE WHEN EXCLUDED.biggest_value > kill_entity_daily_rebuild.biggest_value THEN EXCLUDED.biggest_killmail_id ELSE kill_entity_daily_rebuild.biggest_killmail_id END,
    biggest_value = GREATEST(kill_entity_daily_rebuild.biggest_value, EXCLUDED.biggest_value);

-- name: RebuildKillEntityDailySystemsChunk :exec
WITH k AS MATERIALIZED (
    SELECT kl.killmail_id, (kl.killmail_time AT TIME ZONE 'UTC')::date AS day, kl.npc, kl.system_id,
        kl.victim_character_id AS vc, kl.victim_corporation_id AS vco, kl.victim_alliance_id AS val
    FROM kills kl
    WHERE kl.public AND kl.killmail_id > sqlc.arg(after_id)::bigint AND kl.killmail_id <= sqlc.arg(through_id)::bigint
      AND kl.system_id > 0 AND NOT kl.npc
      AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = kl.killmail_id)
), src AS MATERIALIZED (
    SELECT k.killmail_id, k.vc, k.vco, k.val, k.day, k.system_id, t.character_id, t.corporation_id, t.alliance_id
    FROM k CROSS JOIN LATERAL (
        SELECT i.character_id, i.corporation_id, i.alliance_id FROM kill_involvement i WHERE i.killmail_id = k.killmail_id OFFSET 0
    ) t
    WHERE TRUE
), att AS (
    SELECT x.killmail_id, x.day, x.system_id, 'character'::text AS kind, x.character_id AS id FROM src x
    WHERE x.character_id > 0 AND x.character_id <> x.vc
    UNION
    SELECT x.killmail_id, x.day, x.system_id, 'corporation'::text AS kind, x.corporation_id AS id FROM src x
    WHERE x.corporation_id > 0 AND x.corporation_id <> x.vco
    UNION
    SELECT x.killmail_id, x.day, x.system_id, 'alliance'::text AS kind, x.alliance_id AS id FROM src x
    WHERE x.alliance_id > 0 AND x.alliance_id <> x.val
)
INSERT INTO kill_entity_daily_systems_rebuild (day, kind, entity_id, system_id, events)
SELECT att.day, att.kind, att.id, att.system_id, COUNT(*)::bigint
FROM att
GROUP BY att.day, att.kind, att.id, att.system_id
ON CONFLICT (day, kind, entity_id, system_id) DO UPDATE SET
    events = kill_entity_daily_systems_rebuild.events + EXCLUDED.events;

-- name: RebuildKillEntityDailyShipsChunk :exec
WITH k AS MATERIALIZED (
    SELECT kl.killmail_id, (kl.killmail_time AT TIME ZONE 'UTC')::date AS day, kl.npc, kl.ship_type_id,
        kl.victim_character_id AS vc, kl.victim_corporation_id AS vco, kl.victim_alliance_id AS val
    FROM kills kl
    WHERE kl.public AND kl.killmail_id > sqlc.arg(after_id)::bigint AND kl.killmail_id <= sqlc.arg(through_id)::bigint
      AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = kl.killmail_id)
), src AS MATERIALIZED (
    SELECT k.killmail_id, k.vc, k.vco, k.val, k.day, t.character_id, t.corporation_id, t.alliance_id, t.ship_type_id
    FROM k CROSS JOIN LATERAL (
        SELECT a.character_id, a.corporation_id, a.alliance_id, a.ship_type_id FROM kill_attackers a WHERE a.killmail_id = k.killmail_id OFFSET 0
    ) t
    WHERE NOT k.npc AND t.ship_type_id > 0
), flown AS (
    SELECT x.killmail_id, x.day, x.ship_type_id, 'character'::text AS kind, x.character_id AS id FROM src x
    WHERE x.character_id > 0 AND x.character_id <> x.vc
    UNION
    SELECT x.killmail_id, x.day, x.ship_type_id, 'corporation'::text AS kind, x.corporation_id AS id FROM src x
    WHERE x.corporation_id > 0 AND x.corporation_id <> x.vco
    UNION
    SELECT x.killmail_id, x.day, x.ship_type_id, 'alliance'::text AS kind, x.alliance_id AS id FROM src x
    WHERE x.alliance_id > 0 AND x.alliance_id <> x.val
), r AS (
    SELECT f.day, f.kind, f.id AS entity_id, f.ship_type_id, 1::bigint AS flown, 0::bigint AS lost FROM flown f
    UNION ALL
    SELECT k.day, 'character'::text, k.vc, k.ship_type_id, 0::bigint, 1::bigint
    FROM k
    WHERE k.vc > 0 AND k.ship_type_id > 0
    UNION ALL
    SELECT k.day, 'corporation'::text, k.vco, k.ship_type_id, 0::bigint, 1::bigint
    FROM k
    WHERE k.vco > 0 AND k.ship_type_id > 0
    UNION ALL
    SELECT k.day, 'alliance'::text, k.val, k.ship_type_id, 0::bigint, 1::bigint
    FROM k
    WHERE k.val > 0 AND k.ship_type_id > 0
)
INSERT INTO kill_entity_daily_ships_rebuild (day, kind, entity_id, ship_type_id, flown, lost)
SELECT r.day, r.kind, r.entity_id, r.ship_type_id, SUM(r.flown)::bigint, SUM(r.lost)::bigint
FROM r
GROUP BY r.day, r.kind, r.entity_id, r.ship_type_id
ON CONFLICT (day, kind, entity_id, ship_type_id) DO UPDATE SET
    flown = kill_entity_daily_ships_rebuild.flown + EXCLUDED.flown,
    lost = kill_entity_daily_ships_rebuild.lost + EXCLUDED.lost;

-- name: GetKillStatsRebuildState :one
SELECT cursor, started_at, done_at FROM kill_stats_rebuild_state WHERE id = 1;

-- The archive restore's walk (killboard_archive.go).
-- name: GetKillArchiveRestoreState :one
SELECT day, done_at FROM kill_archive_restore_state WHERE id = 1;

-- name: SetKillArchiveRestoreDay :exec
UPDATE kill_archive_restore_state SET day = sqlc.arg(day)::date WHERE id = 1;

-- name: MarkKillArchiveRestoreDone :exec
UPDATE kill_archive_restore_state SET done_at = now() WHERE id = 1;

-- One UTC day's counted kills that have no attacker rows: the
-- archive restore reads that day's file for them. Kills still
-- waiting in kill_stats_pending are the live fold's.
-- name: ListDayKillsMissingAttackers :many
SELECT k.killmail_id, k.killmail_time FROM kills k
WHERE k.killmail_time >= sqlc.arg(day_start)::timestamptz AND k.killmail_time < sqlc.arg(day_end)::timestamptz
  AND k.attacker_count > 0
  AND NOT EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id)
  AND NOT EXISTS (SELECT 1 FROM kill_stats_pending p WHERE p.killmail_id = k.killmail_id);

-- Ships flown for kills whose attacker rows were just written back,
-- added to the live daily aggregate under the rebuild's rules
-- (RebuildKillEntityDailyShipsChunk): public non-NPC kills, hulls
-- known, the victim's own character, corporation or alliance not
-- credited. Losses were counted from the kill row already.
-- name: AddKillShipsFlown :exec
WITH k AS MATERIALIZED (
    SELECT kl.killmail_id, (kl.killmail_time AT TIME ZONE 'UTC')::date AS day,
        kl.victim_character_id AS vc, kl.victim_corporation_id AS vco, kl.victim_alliance_id AS val
    FROM kills kl
    WHERE kl.killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]) AND kl.public AND NOT kl.npc
), src AS (
    SELECT k.killmail_id, k.day, k.vc, k.vco, k.val, a.character_id, a.corporation_id, a.alliance_id, a.ship_type_id
    FROM k JOIN kill_attackers a ON a.killmail_id = k.killmail_id
    WHERE a.ship_type_id > 0
), flown AS (
    SELECT x.killmail_id, x.day, x.ship_type_id, 'character'::text AS kind, x.character_id AS id FROM src x
    WHERE x.character_id > 0 AND x.character_id <> x.vc
    UNION
    SELECT x.killmail_id, x.day, x.ship_type_id, 'corporation'::text AS kind, x.corporation_id AS id FROM src x
    WHERE x.corporation_id > 0 AND x.corporation_id <> x.vco
    UNION
    SELECT x.killmail_id, x.day, x.ship_type_id, 'alliance'::text AS kind, x.alliance_id AS id FROM src x
    WHERE x.alliance_id > 0 AND x.alliance_id <> x.val
)
INSERT INTO kill_entity_daily_ships (day, kind, entity_id, ship_type_id, flown, lost)
SELECT f.day, f.kind, f.id, f.ship_type_id, COUNT(*)::bigint, 0::bigint
FROM flown f
GROUP BY f.day, f.kind, f.id, f.ship_type_id
ON CONFLICT (day, kind, entity_id, ship_type_id) DO UPDATE SET
    flown = kill_entity_daily_ships.flown + EXCLUDED.flown;

-- Attacker rows written back only to count a kill past retention go
-- again in the same transaction: past retention a kill keeps its
-- counted stats, not its detail.
-- name: DeleteKillAttackersFor :exec
DELETE FROM kill_attackers WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: StartKillStatsRebuild :exec
UPDATE kill_stats_rebuild_state SET cursor = 0, started_at = sqlc.arg(started_at)::timestamptz, done_at = NULL WHERE id = 1;

-- name: SetKillStatsRebuildCursor :exec
UPDATE kill_stats_rebuild_state SET cursor = sqlc.arg(cursor)::bigint WHERE id = 1;

-- One feed row per (entity, kill), written by the fold at
-- ingest. The board's filters ride the stored columns.
-- name: InsertKillEntityFeed :exec
INSERT INTO kill_entity_feed (kind, entity_id, killmail_time, killmail_id, solo, value)
VALUES (sqlc.arg(kind)::text, sqlc.arg(entity_id)::bigint, sqlc.arg(killmail_time)::timestamptz,
    sqlc.arg(killmail_id)::bigint, sqlc.arg(solo)::boolean, sqlc.arg(value)::double precision)
ON CONFLICT DO NOTHING;

-- An entity's kills and losses, newest first: one keyset scan
-- on the feed's page index, then the page's kills by primary
-- key. This replaces the per kind ListKillsBy queries, whose
-- materialized involvement CTE read an entity's whole history
-- on every page.
-- name: ListKillsByEntityFeed :many
SELECT k.* FROM (
    SELECT f.killmail_id, f.killmail_time FROM kill_entity_feed f
    WHERE f.kind = sqlc.arg(kind)::text AND f.entity_id = sqlc.arg(entity_id)::bigint
      AND (f.killmail_time, f.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
      AND f.value >= sqlc.arg(min_value)::double precision
      AND (NOT sqlc.arg(solo_only)::boolean OR f.solo)
    ORDER BY f.killmail_time DESC, f.killmail_id DESC
    LIMIT sqlc.arg(max_rows)::bigint
) p
JOIN kills k ON k.killmail_id = p.killmail_id AND k.public
ORDER BY p.killmail_time DESC, p.killmail_id DESC;

-- The list backfill's state: where its walk stands, and whether
-- it has finished.
-- name: GetKillFeedBackfillState :one
SELECT cursor_time, cursor_id, done_at FROM kill_feed_backfill_state WHERE id = 1;

-- name: SetKillFeedBackfillCursor :exec
UPDATE kill_feed_backfill_state SET cursor_time = sqlc.arg(cursor_time)::timestamptz,
    cursor_id = sqlc.arg(cursor_id)::bigint WHERE id = 1;

-- name: MarkKillFeedBackfillDone :exec
UPDATE kill_feed_backfill_state SET done_at = sqlc.arg(done_at)::timestamptz WHERE id = 1;

-- The next kills after the backfill's cursor, in (time, id) order
-- through the time index, each with whether it already has feed
-- rows: one bounded batch per pass, never a scan of the table.
-- name: ListKillsAfterFeedCursor :many
SELECT k.killmail_id, k.killmail_time,
    EXISTS (SELECT 1 FROM kill_entity_feed f WHERE f.killmail_id = k.killmail_id) AS has_feed
FROM kills k
WHERE k.killmail_time >= sqlc.arg(cursor_time)::timestamptz
  AND (k.killmail_time, k.killmail_id) > (sqlc.arg(cursor_time)::timestamptz, sqlc.arg(cursor_id)::bigint)
ORDER BY k.killmail_time, k.killmail_id
LIMIT sqlc.arg(max_rows)::bigint;

-- name: UpsertKillboardHeadline :exec
INSERT INTO killboard_headline (id, kills_hour, kills_day, isk_day, biggest_killmail_id, computed_at)
VALUES (1, sqlc.arg(kills_hour)::bigint, sqlc.arg(kills_day)::bigint, sqlc.arg(isk_day)::double precision,
    sqlc.arg(biggest_killmail_id)::bigint, sqlc.arg(computed_at)::timestamptz)
ON CONFLICT (id) DO UPDATE SET
    kills_hour = EXCLUDED.kills_hour, kills_day = EXCLUDED.kills_day, isk_day = EXCLUDED.isk_day,
    biggest_killmail_id = EXCLUDED.biggest_killmail_id, computed_at = EXCLUDED.computed_at;

-- name: GetKillboardHeadline :one
SELECT kills_hour, kills_day, isk_day, biggest_killmail_id, computed_at FROM killboard_headline WHERE id = 1;

-- name: ReplaceKillboardTop :execrows
WITH del AS (DELETE FROM killboard_top WHERE kind = sqlc.arg(kind)::text)
INSERT INTO killboard_top (kind, entity_id, kills, isk)
SELECT sqlc.arg(kind)::text, unnest(sqlc.arg(entity_ids)::bigint[]),
    unnest(sqlc.arg(kills_list)::bigint[]), unnest(sqlc.arg(isk_list)::double precision[])
ON CONFLICT (kind, entity_id) DO UPDATE SET kills = EXCLUDED.kills, isk = EXCLUDED.isk;

-- name: ListKillboardTop :many
SELECT entity_id, kills, isk FROM killboard_top
WHERE kind = sqlc.arg(kind)::text
ORDER BY kills DESC, entity_id
LIMIT sqlc.arg(max_rows)::bigint;
