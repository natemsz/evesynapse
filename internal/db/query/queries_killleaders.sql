-- The killboard's leaderboards (killboard_leaders.go). A run fills
-- the working set with one window's kills and attackers and writes
-- their uncut totals under one day (an EVE day, or the rolling last
-- 24 hours under 1970-01-01); a rank adds up a period's days and keeps a row when its
-- rank by kills or by ISK makes the scope's cut: 50 for New Eden, 5
-- for a system, 10 for every other scope. Values are EveSynapse's own
-- appraisal; an unpriced kill counts with no ISK. Padding kills never
-- reach the working set (FillKillLeadersWin).

-- name: ClearKillLeadersStage :exec
TRUNCATE kill_leaders_win, kill_leaders_att;

-- The window's kills, padding left out: a loss worth under
-- padding_value (an empty capsule, rookie ship, shuttle or bare hull:
-- nothing fitted or plugged in; an unpriced kill counts as empty),
-- a ship its own pilot helped kill, and a kill where every pilot on
-- the attacker list flies for the victim's own player corporation
-- (NPC corporations, ids under 2000000, hold strangers who fight).
-- One read of each kill's attacker rows answers both attacker tests.
-- name: FillKillLeadersWin :execrows
INSERT INTO kill_leaders_win (killmail_id, system_id, region_id, ship_type_id, value, solo, v_character, v_corporation, v_alliance)
SELECT k.killmail_id, k.system_id, k.region_id, k.ship_type_id, COALESCE(k.value_ours, 0), k.solo,
       k.victim_character_id, k.victim_corporation_id, k.victim_alliance_id
FROM kills k
CROSS JOIN LATERAL (
    SELECT COALESCE(bool_or(a.character_id = k.victim_character_id), false) AS own_pilot,
           COALESCE(bool_or(a.character_id > 0), false) AS any_pilot,
           COALESCE(bool_or(a.character_id > 0 AND a.corporation_id <> k.victim_corporation_id), false) AS outside_pilot
    FROM kill_attackers a WHERE a.killmail_id = k.killmail_id
) f
WHERE k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz AND k.killmail_time < sqlc.arg(until)::timestamptz
  AND COALESCE(k.value_ours, 0) >= sqlc.arg(padding_value)::double precision
  AND NOT (k.victim_character_id > 0 AND f.own_pilot)
  AND NOT (k.victim_corporation_id >= 2000000 AND f.any_pilot AND NOT f.outside_pilot);

-- name: FillKillLeadersAtt :exec
INSERT INTO kill_leaders_att (killmail_id, character_id, corporation_id, alliance_id, ship_type_id)
SELECT DISTINCT a.killmail_id, a.character_id, a.corporation_id, a.alliance_id, a.ship_type_id
FROM kill_leaders_win w
JOIN kill_attackers a ON a.killmail_id = w.killmail_id
WHERE a.character_id > 0 OR a.corporation_id > 0;

-- name: DeleteKillLeadersDay :exec
DELETE FROM kill_leaders_totals WHERE day = sqlc.arg(day)::date;

-- Attackers: pilots (board 1), corporations (2) or alliances (3), in
-- New Eden, each region and system, each hull they flew, and (for
-- pilots and corporations) their own alliance or corporation. An
-- attacker on the victim's own side of the same kind does not count,
-- as in the daily stats fold.
-- name: InsertKillLeadersTotalsEntity :exec
INSERT INTO kill_leaders_totals (day, board, scope, scope_id, entity_id, kills, solo, isk)
SELECT sqlc.arg(day)::date, sqlc.arg(board)::smallint, p.scope, p.scope_id, p.entity_id, count(*)::integer,
       (count(*) FILTER (WHERE w.solo))::integer, COALESCE(sum(w.value), 0)::double precision
FROM (
    SELECT DISTINCT s.scope, s.scope_id, e.killmail_id, e.entity_id
    FROM (
        SELECT a.killmail_id, a.ship_type_id, a.corporation_id, a.alliance_id,
               CASE sqlc.arg(board)::smallint WHEN 1 THEN a.character_id
                                              WHEN 2 THEN a.corporation_id
                                              ELSE a.alliance_id END AS entity_id
        FROM kill_leaders_att a
    ) e
    JOIN kill_leaders_win w ON w.killmail_id = e.killmail_id
    CROSS JOIN LATERAL (VALUES (0::smallint, 0::bigint), (1::smallint, w.region_id), (2::smallint, w.system_id),
                               (3::smallint, e.ship_type_id), (4::smallint, e.alliance_id), (5::smallint, e.corporation_id)) s(scope, scope_id)
    WHERE e.entity_id > 0
      AND e.entity_id <> CASE sqlc.arg(board)::smallint WHEN 1 THEN w.v_character
                                                        WHEN 2 THEN w.v_corporation
                                                        ELSE w.v_alliance END
      AND (s.scope = 0 OR s.scope_id > 0)
      AND (s.scope <> 4 OR sqlc.arg(board)::smallint IN (1, 2))
      AND (s.scope <> 5 OR sqlc.arg(board)::smallint = 1)
) p
JOIN kill_leaders_win w ON w.killmail_id = p.killmail_id
GROUP BY p.scope, p.scope_id, p.entity_id;

-- Victim-side boards: hulls destroyed (board 4, in New Eden, each
-- region and system), systems (6, New Eden and each region) and
-- regions (7, New Eden).
-- name: InsertKillLeadersTotalsPlace :exec
INSERT INTO kill_leaders_totals (day, board, scope, scope_id, entity_id, kills, solo, isk)
SELECT sqlc.arg(day)::date, sqlc.arg(board)::smallint, s.scope, s.scope_id,
       CASE sqlc.arg(board)::smallint WHEN 4 THEN w.ship_type_id
                                      WHEN 6 THEN w.system_id
                                      ELSE w.region_id END AS entity_id,
       count(*)::integer, (count(*) FILTER (WHERE w.solo))::integer, COALESCE(sum(w.value), 0)::double precision
FROM kill_leaders_win w
CROSS JOIN LATERAL (VALUES (0::smallint, 0::bigint), (1::smallint, w.region_id), (2::smallint, w.system_id)) s(scope, scope_id)
WHERE (s.scope = 0
       OR (s.scope = 1 AND s.scope_id > 0 AND sqlc.arg(board)::smallint <> 7)
       OR (s.scope = 2 AND sqlc.arg(board)::smallint = 4))
  AND (CASE sqlc.arg(board)::smallint WHEN 4 THEN w.ship_type_id
                                      WHEN 6 THEN w.system_id
                                      ELSE w.region_id END) > 0
GROUP BY 1, 2, 3, 4, 5;

-- Single kills by value (board 8), in New Eden, each region and
-- system: each scope's biggest only, as deep as its cut.
-- name: InsertKillLeadersTotalsKills :exec
INSERT INTO kill_leaders_totals (day, board, scope, scope_id, entity_id, kills, solo, isk)
SELECT sqlc.arg(day)::date, 8, r.scope, r.scope_id, r.killmail_id, 1, r.solo, r.value
FROM (
    SELECT s.scope, s.scope_id, w.killmail_id, w.solo::int AS solo, w.value,
           row_number() OVER (PARTITION BY s.scope, s.scope_id ORDER BY w.value DESC, w.killmail_id) AS rn
    FROM kill_leaders_win w
    CROSS JOIN LATERAL (VALUES (0::smallint, 0::bigint), (1::smallint, w.region_id), (2::smallint, w.system_id)) s(scope, scope_id)
    WHERE s.scope = 0 OR s.scope_id > 0
) r
WHERE r.rn <= CASE r.scope WHEN 0 THEN 50 WHEN 2 THEN 5 ELSE 10 END;

-- Hulls flown by attacking pilots (board 5), in New Eden and each
-- region: the kills each hull was on. A capsule on the attacker list
-- is a pilot whose drones or fighters landed after the ship died: not
-- a hull anyone flew into the fight, so capsules stay off this board.
-- name: InsertKillLeadersTotalsShipsUsed :exec
INSERT INTO kill_leaders_totals (day, board, scope, scope_id, entity_id, kills, solo, isk)
SELECT sqlc.arg(day)::date, 5, p.scope, p.scope_id, p.entity_id, count(*)::integer,
       (count(*) FILTER (WHERE w.solo))::integer, COALESCE(sum(w.value), 0)::double precision
FROM (
    SELECT DISTINCT s.scope, s.scope_id, a.killmail_id, a.ship_type_id AS entity_id
    FROM kill_leaders_att a
    JOIN kill_leaders_win w ON w.killmail_id = a.killmail_id
    CROSS JOIN LATERAL (VALUES (0::smallint, 0::bigint), (1::smallint, w.region_id)) s(scope, scope_id)
    WHERE a.character_id > 0 AND a.ship_type_id > 0 AND a.ship_type_id NOT IN (670, 33328)
      AND (s.scope = 0 OR s.scope_id > 0)
) p
JOIN kill_leaders_win w ON w.killmail_id = p.killmail_id
GROUP BY p.scope, p.scope_id, p.entity_id;

-- name: UpsertKillLeadersDay :exec
INSERT INTO kill_leaders_days (day, computed_at)
VALUES (sqlc.arg(day)::date, sqlc.arg(computed_at)::timestamptz)
ON CONFLICT (day) DO UPDATE SET computed_at = EXCLUDED.computed_at;

-- name: ListKillLeadersDays :many
SELECT day, computed_at FROM kill_leaders_days;

-- Days older than the longest period go; the rolling day stays.
-- name: PruneKillLeadersDays :exec
WITH gone AS (
    DELETE FROM kill_leaders_days WHERE day > DATE '1970-01-01' AND day < sqlc.arg(oldest)::date RETURNING day
)
DELETE FROM kill_leaders_totals t USING gone WHERE t.day = gone.day;

-- name: DeleteKillLeadersPeriod :exec
DELETE FROM kill_leaders WHERE period = sqlc.arg(period)::text;

-- A period's ranks: its days added up per board, scope and entity,
-- each scope cut by its depth, written with the names the pages read.
-- name: RankKillLeaders :exec
INSERT INTO kill_leaders (period, scope_kind, scope_id, board, entity_id, kills, solo, isk, rank_kills, rank_isk)
SELECT sqlc.arg(period)::text,
       (ARRAY['', 'region', 'system', 'ship', 'alliance', 'corporation'])[r.scope + 1],
       r.scope_id,
       (ARRAY['character', 'corporation', 'alliance', 'ship_lost', 'ship_used', 'system', 'region', 'kill'])[r.board],
       r.entity_id, r.kills, r.solo, r.isk, r.rk, r.ri
FROM (
    SELECT g.board, g.scope, g.scope_id, g.entity_id, g.kills, g.solo, g.isk,
           row_number() OVER (PARTITION BY g.board, g.scope, g.scope_id ORDER BY g.kills DESC, g.isk DESC, g.entity_id)::integer AS rk,
           row_number() OVER (PARTITION BY g.board, g.scope, g.scope_id ORDER BY g.isk DESC, g.kills DESC, g.entity_id)::integer AS ri
    FROM (
        SELECT t.board, t.scope, t.scope_id, t.entity_id, sum(t.kills)::bigint AS kills,
               sum(t.solo)::bigint AS solo, sum(t.isk)::double precision AS isk
        FROM kill_leaders_totals t
        WHERE t.day = ANY(sqlc.arg(days)::date[])
        GROUP BY t.board, t.scope, t.scope_id, t.entity_id
    ) g
) r
WHERE LEAST(r.rk, r.ri) <= CASE r.scope WHEN 0 THEN 50 WHEN 2 THEN 5 ELSE 10 END;

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
