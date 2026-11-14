-- Sov fights (sov_fights.go): the contests the campaigns mirror has
-- seen and the kills fought in them. Every read is capped.

-- Every contest in the mirror now: new ones start a fight, known ones
-- refresh their scores and timer, and one ESI lists again reopens.
-- name: SyncSovFights :exec
INSERT INTO sov_fights (campaign_id, system_id, constellation_id, region_id, structure_id, event_type,
    defender_id, start_time, first_seen, last_seen, defender_score, attackers_score)
SELECT c.campaign_id, c.system_id, COALESCE(NULLIF(c.constellation_id, 0), s.constellation_id, 0),
       COALESCE(s.region_id, 0), c.structure_id, c.event_type, c.defender_id, c.start_time,
       sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz, c.defender_score, c.attackers_score
FROM sov_campaigns c
LEFT JOIN sde_systems s ON s.system_id = c.system_id
ON CONFLICT (campaign_id) DO UPDATE SET
    start_time = EXCLUDED.start_time, last_seen = EXCLUDED.last_seen,
    defender_score = EXCLUDED.defender_score, attackers_score = EXCLUDED.attackers_score,
    ended_at = NULL;

-- A fight ESI no longer lists has ended.
-- name: EndSovFights :execrows
UPDATE sov_fights SET ended_at = sqlc.arg(now)::timestamptz
WHERE ended_at IS NULL AND campaign_id NOT IN (SELECT campaign_id FROM sov_campaigns);

-- The open fights, for the in-memory index the kill ingest reads.
-- name: ListOpenSovFights :many
SELECT campaign_id, constellation_id, defender_id, start_time FROM sov_fights
WHERE ended_at IS NULL AND constellation_id > 0;

-- One kill into one fight, at ingest.
-- name: InsertSovFightKill :exec
INSERT INTO sov_fight_kills (campaign_id, killmail_id, killmail_time, system_id, side)
VALUES (sqlc.arg(campaign_id)::bigint, sqlc.arg(killmail_id)::bigint, sqlc.arg(killmail_time)::timestamptz,
        sqlc.arg(system_id)::bigint, sqlc.arg(side)::smallint)
ON CONFLICT DO NOTHING;

-- The catch-up: every stored kill the defending alliance took part
-- in (its ship lost, or on the attacker list) in an open or just-ended
-- fight's constellation and window, through each system's kill index.
-- It files kills stored before the fight was seen and any the ingest
-- missed.
-- name: CorrelateSovFightKills :execrows
INSERT INTO sov_fight_kills (campaign_id, killmail_id, killmail_time, system_id, side)
SELECT f.campaign_id, k.killmail_id, k.killmail_time, k.system_id,
       (CASE WHEN k.victim_alliance_id = f.defender_id THEN 1 ELSE 2 END)::smallint
FROM sov_fights f
JOIN sde_systems s ON s.constellation_id = f.constellation_id
JOIN kills k ON k.system_id = s.system_id
    AND k.killmail_time >= f.start_time - make_interval(secs => sqlc.arg(lead_secs)::double precision)
    AND k.killmail_time < COALESCE(f.ended_at, 'infinity'::timestamptz)
WHERE f.constellation_id > 0 AND f.defender_id > 0
  AND (f.ended_at IS NULL OR f.ended_at >= sqlc.arg(since)::timestamptz)
  AND k.public AND NOT k.npc
  AND (k.victim_alliance_id = f.defender_id
       OR EXISTS (SELECT 1 FROM kill_involvement i WHERE i.killmail_id = k.killmail_id AND i.alliance_id = f.defender_id))
ON CONFLICT DO NOTHING;

-- The stored figures of the open and just-ended fights, recounted
-- from their kills (a price that lands late reaches the ISK).
-- name: RefreshSovFightStats :exec
UPDATE sov_fights f SET
    kills = a.kills, isk = a.isk,
    defender_lost = a.dl, defender_lost_isk = a.dli,
    defender_killed = a.dk, defender_killed_isk = a.dki,
    last_kill_at = a.last_at
FROM (
    SELECT fk.campaign_id, count(*)::bigint AS kills, COALESCE(sum(k.value_ours), 0)::double precision AS isk,
           count(*) FILTER (WHERE fk.side = 1)::bigint AS dl,
           COALESCE(sum(k.value_ours) FILTER (WHERE fk.side = 1), 0)::double precision AS dli,
           count(*) FILTER (WHERE fk.side = 2)::bigint AS dk,
           COALESCE(sum(k.value_ours) FILTER (WHERE fk.side = 2), 0)::double precision AS dki,
           max(fk.killmail_time) AS last_at
    FROM sov_fight_kills fk
    JOIN kills k ON k.killmail_id = fk.killmail_id
    WHERE fk.campaign_id IN (SELECT campaign_id FROM sov_fights WHERE ended_at IS NULL OR ended_at >= sqlc.arg(since)::timestamptz)
    GROUP BY fk.campaign_id
) a
WHERE f.campaign_id = a.campaign_id;

-- The figures of the contests a page shows.
-- name: ListSovFightStats :many
SELECT campaign_id, kills, isk, defender_lost, defender_lost_isk, defender_killed, defender_killed_isk, last_kill_at
FROM sov_fights WHERE campaign_id = ANY(sqlc.arg(ids)::bigint[]);

-- One fight's newest kills, with the defender's side in each.
-- name: ListSovFightKillsOf :many
SELECT sqlc.embed(k), fk.side
FROM sov_fight_kills fk
JOIN kills k ON k.killmail_id = fk.killmail_id AND k.public
WHERE fk.campaign_id = sqlc.arg(campaign_id)::bigint
ORDER BY fk.killmail_time DESC, fk.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- One fight's figures and place.
-- name: GetSovFightFigures :one
SELECT f.campaign_id, f.system_id, ss.name AS system_name, f.event_type, f.ended_at,
       f.kills, f.isk, f.defender_lost, f.defender_lost_isk, f.defender_killed, f.defender_killed_isk
FROM sov_fights f JOIN sde_systems ss ON ss.system_id = f.system_id
WHERE f.campaign_id = sqlc.arg(campaign_id)::bigint;

-- Fights that ended lately, newest first, with the system's holder
-- now.
-- name: ListSovFightsEnded :many
SELECT f.campaign_id, f.system_id, ss.name AS system_name, ss.security, COALESCE(r.name, '')::text AS region_name,
       f.event_type, f.defender_id, f.start_time, f.ended_at, f.defender_score, f.attackers_score,
       f.kills, f.isk, f.defender_lost, f.defender_lost_isk, f.defender_killed, f.defender_killed_isk,
       COALESCE(so.alliance_id, 0)::bigint AS holder_id
FROM sov_fights f
JOIN sde_systems ss ON ss.system_id = f.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
LEFT JOIN system_sovereignty so ON so.system_id = f.system_id
WHERE f.ended_at IS NOT NULL AND f.ended_at >= sqlc.arg(since)::timestamptz
  AND (sqlc.arg(region_id)::bigint = 0 OR f.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(defender_id)::bigint = 0 OR f.defender_id = sqlc.arg(defender_id)::bigint)
ORDER BY f.ended_at DESC, f.campaign_id
LIMIT sqlc.arg(max_rows)::bigint;

-- One fight, for the killboard's fight scope.
-- name: GetSovFight :one
SELECT f.campaign_id, f.system_id, ss.name AS system_name, f.event_type, f.defender_id, f.start_time, f.ended_at
FROM sov_fights f JOIN sde_systems ss ON ss.system_id = f.system_id
WHERE f.campaign_id = sqlc.arg(campaign_id)::bigint;

-- A fight's kills on the killboard, newest first, keyset paged.
-- name: ListKillsByFight :many
SELECT k.* FROM sov_fight_kills fk
JOIN kills k ON k.killmail_id = fk.killmail_id
WHERE fk.campaign_id = sqlc.arg(campaign_id)::bigint
  AND (fk.killmail_time, fk.killmail_id) < (sqlc.arg(before_time)::timestamptz, sqlc.arg(before_id)::bigint)
  AND k.public
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(min_value)::double precision
  AND (NOT sqlc.arg(solo_only)::boolean OR k.solo)
ORDER BY fk.killmail_time DESC, fk.killmail_id DESC
LIMIT sqlc.arg(max_rows)::bigint;
