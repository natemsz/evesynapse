-- The killboard's leaderboards (killboard_leaders.go). A run fills
-- the working set with one period's kills and attackers, then ranks
-- each board for every scope in one statement per board, keeping a
-- row when its rank by kills or by ISK makes the scope's cut: 50 for
-- New Eden, 5 for a system, 10 for every other scope. Values are
-- EveSynapse's own appraisal; an unpriced kill counts with no ISK.
-- Padding kills never reach the working set (FillKillLeadersWin).

-- name: ClearKillLeadersStage :exec
TRUNCATE kill_leaders_win, kill_leaders_att;

-- The period's kills, padding left out: a loss worth under
-- padding_value (an empty capsule, rookie ship, shuttle or bare hull:
-- nothing fitted or plugged in; an unpriced kill counts as empty),
-- a ship its own pilot helped kill, and a kill where every pilot on
-- the attacker list flies for the victim's own player corporation
-- (NPC corporations, ids under 2000000, hold strangers who fight).
-- name: FillKillLeadersWin :execrows
INSERT INTO kill_leaders_win (killmail_id, system_id, region_id, ship_type_id, value, solo, v_character, v_corporation, v_alliance)
SELECT k.killmail_id, k.system_id, k.region_id, k.ship_type_id, COALESCE(k.value_ours, 0), k.solo,
       k.victim_character_id, k.victim_corporation_id, k.victim_alliance_id
FROM kills k
WHERE k.public AND NOT k.npc AND k.killmail_time >= sqlc.arg(since)::timestamptz
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(padding_value)::double precision
  AND NOT (k.victim_character_id > 0 AND EXISTS (
        SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id AND a.character_id = k.victim_character_id))
  AND NOT (k.victim_corporation_id >= 2000000
        AND EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id AND a.character_id > 0)
        AND NOT EXISTS (SELECT 1 FROM kill_attackers a WHERE a.killmail_id = k.killmail_id AND a.character_id > 0
                          AND a.corporation_id <> k.victim_corporation_id));

-- name: FillKillLeadersAtt :exec
INSERT INTO kill_leaders_att (killmail_id, character_id, corporation_id, alliance_id, ship_type_id)
SELECT DISTINCT a.killmail_id, a.character_id, a.corporation_id, a.alliance_id, a.ship_type_id
FROM kill_leaders_win w
JOIN kill_attackers a ON a.killmail_id = w.killmail_id
WHERE a.character_id > 0 OR a.corporation_id > 0;

-- name: DeleteKillLeadersPeriod :exec
DELETE FROM kill_leaders WHERE period = sqlc.arg(period)::text;

-- Attackers: pilots, corporations or alliances (board), in New Eden,
-- each region and system, each hull they flew, and (for pilots and
-- corporations) their own alliance or corporation. An attacker on
-- the victim's own side of the same kind does not count, as in the
-- daily stats fold.
-- name: InsertKillLeadersEntity :exec
INSERT INTO kill_leaders (period, scope_kind, scope_id, board, entity_id, kills, solo, isk, rank_kills, rank_isk)
SELECT sqlc.arg(period)::text, r.scope_kind, r.scope_id, sqlc.arg(board)::text, r.entity_id, r.kills, r.solo, r.isk, r.rk, r.ri
FROM (
    SELECT g.scope_kind, g.scope_id, g.entity_id, g.kills, g.solo, g.isk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.kills DESC, g.isk DESC, g.entity_id)::integer AS rk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.isk DESC, g.kills DESC, g.entity_id)::integer AS ri
    FROM (
        SELECT p.scope_kind, p.scope_id, p.entity_id, count(*)::bigint AS kills,
               count(*) FILTER (WHERE w.solo)::bigint AS solo, COALESCE(sum(w.value), 0)::double precision AS isk
        FROM (
            SELECT DISTINCT s.scope_kind, s.scope_id, e.killmail_id, e.entity_id
            FROM (
                SELECT a.killmail_id, a.ship_type_id, a.corporation_id, a.alliance_id,
                       CASE sqlc.arg(board)::text WHEN 'character' THEN a.character_id
                                                  WHEN 'corporation' THEN a.corporation_id
                                                  ELSE a.alliance_id END AS entity_id
                FROM kill_leaders_att a
            ) e
            JOIN kill_leaders_win w ON w.killmail_id = e.killmail_id
            CROSS JOIN LATERAL (VALUES ('', 0::bigint), ('region', w.region_id), ('system', w.system_id),
                                       ('ship', e.ship_type_id), ('alliance', e.alliance_id), ('corporation', e.corporation_id)) s(scope_kind, scope_id)
            WHERE e.entity_id > 0
              AND e.entity_id <> CASE sqlc.arg(board)::text WHEN 'character' THEN w.v_character
                                                            WHEN 'corporation' THEN w.v_corporation
                                                            ELSE w.v_alliance END
              AND (s.scope_kind = '' OR s.scope_id > 0)
              AND (s.scope_kind <> 'alliance' OR sqlc.arg(board)::text IN ('character', 'corporation'))
              AND (s.scope_kind <> 'corporation' OR sqlc.arg(board)::text = 'character')
        ) p
        JOIN kill_leaders_win w ON w.killmail_id = p.killmail_id
        GROUP BY p.scope_kind, p.scope_id, p.entity_id
    ) g
) r
WHERE LEAST(r.rk, r.ri) <= CASE r.scope_kind WHEN '' THEN 50 WHEN 'system' THEN 5 ELSE 10 END;

-- Victim-side boards: hulls destroyed ('ship_lost', in New Eden,
-- each region and system), systems ('system', New Eden and each
-- region), regions ('region', New Eden) and single kills ('kill', in
-- New Eden, each region and system).
-- name: InsertKillLeadersPlace :exec
INSERT INTO kill_leaders (period, scope_kind, scope_id, board, entity_id, kills, solo, isk, rank_kills, rank_isk)
SELECT sqlc.arg(period)::text, r.scope_kind, r.scope_id, sqlc.arg(board)::text, r.entity_id, r.kills, r.solo, r.isk, r.rk, r.ri
FROM (
    SELECT g.scope_kind, g.scope_id, g.entity_id, g.kills, g.solo, g.isk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.kills DESC, g.isk DESC, g.entity_id)::integer AS rk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.isk DESC, g.kills DESC, g.entity_id)::integer AS ri
    FROM (
        SELECT s.scope_kind, s.scope_id,
               CASE sqlc.arg(board)::text WHEN 'ship_lost' THEN w.ship_type_id
                                          WHEN 'system' THEN w.system_id
                                          WHEN 'region' THEN w.region_id
                                          ELSE w.killmail_id END AS entity_id,
               count(*)::bigint AS kills, count(*) FILTER (WHERE w.solo)::bigint AS solo,
               COALESCE(sum(w.value), 0)::double precision AS isk
        FROM kill_leaders_win w
        CROSS JOIN LATERAL (VALUES ('', 0::bigint), ('region', w.region_id), ('system', w.system_id)) s(scope_kind, scope_id)
        WHERE (s.scope_kind = ''
               OR (s.scope_kind = 'region' AND s.scope_id > 0 AND sqlc.arg(board)::text <> 'region')
               OR (s.scope_kind = 'system' AND sqlc.arg(board)::text IN ('ship_lost', 'kill')))
        GROUP BY 1, 2, 3
    ) g
    WHERE g.entity_id > 0
) r
WHERE LEAST(r.rk, r.ri) <= CASE r.scope_kind WHEN '' THEN 50 WHEN 'system' THEN 5 ELSE 10 END;

-- Hulls flown by attacking pilots, in New Eden and each region: the
-- kills each hull was on. A capsule on the attacker list is a pilot
-- whose drones or fighters landed after the ship died: not a hull
-- anyone flew into the fight, so capsules stay off this board.
-- name: InsertKillLeadersShipsUsed :exec
INSERT INTO kill_leaders (period, scope_kind, scope_id, board, entity_id, kills, solo, isk, rank_kills, rank_isk)
SELECT sqlc.arg(period)::text, r.scope_kind, r.scope_id, 'ship_used', r.entity_id, r.kills, r.solo, r.isk, r.rk, r.ri
FROM (
    SELECT g.scope_kind, g.scope_id, g.entity_id, g.kills, g.solo, g.isk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.kills DESC, g.isk DESC, g.entity_id)::integer AS rk,
           row_number() OVER (PARTITION BY g.scope_kind, g.scope_id ORDER BY g.isk DESC, g.kills DESC, g.entity_id)::integer AS ri
    FROM (
        SELECT p.scope_kind, p.scope_id, p.entity_id, count(*)::bigint AS kills,
               count(*) FILTER (WHERE w.solo)::bigint AS solo, COALESCE(sum(w.value), 0)::double precision AS isk
        FROM (
            SELECT DISTINCT s.scope_kind, s.scope_id, a.killmail_id, a.ship_type_id AS entity_id
            FROM kill_leaders_att a
            JOIN kill_leaders_win w ON w.killmail_id = a.killmail_id
            CROSS JOIN LATERAL (VALUES ('', 0::bigint), ('region', w.region_id)) s(scope_kind, scope_id)
            WHERE a.character_id > 0 AND a.ship_type_id > 0 AND a.ship_type_id NOT IN (670, 33328)
              AND (s.scope_kind = '' OR s.scope_id > 0)
        ) p
        JOIN kill_leaders_win w ON w.killmail_id = p.killmail_id
        GROUP BY p.scope_kind, p.scope_id, p.entity_id
    ) g
) r
WHERE LEAST(r.rk, r.ri) <= CASE r.scope_kind WHEN '' THEN 50 ELSE 10 END;

-- name: UpsertKillLeadersState :exec
INSERT INTO kill_leaders_state (period, since, computed_at, kills)
VALUES (sqlc.arg(period)::text, sqlc.arg(since)::timestamptz, sqlc.arg(computed_at)::timestamptz, sqlc.arg(kills)::bigint)
ON CONFLICT (period) DO UPDATE SET since = EXCLUDED.since, computed_at = EXCLUDED.computed_at, kills = EXCLUDED.kills;

-- name: ListKillLeadersState :many
SELECT period, since, computed_at, kills FROM kill_leaders_state;

-- One board of one scope, best first by kills or by ISK.
-- name: ListKillLeaders :many
SELECT entity_id, kills, solo, isk, rank_kills, rank_isk FROM kill_leaders
WHERE period = sqlc.arg(period)::text AND scope_kind = sqlc.arg(scope_kind)::text
  AND scope_id = sqlc.arg(scope_id)::bigint AND board = sqlc.arg(board)::text
  AND (CASE WHEN sqlc.arg(by_isk)::boolean THEN rank_isk ELSE rank_kills END) <= sqlc.arg(max_rows)::integer
ORDER BY (CASE WHEN sqlc.arg(by_isk)::boolean THEN rank_isk ELSE rank_kills END)
LIMIT sqlc.arg(max_rows)::integer;

-- The kills a page names, by id, public only.
-- name: ListPublicKillsByID :many
SELECT * FROM kills WHERE killmail_id = ANY(sqlc.arg(ids)::bigint[]) AND public;
