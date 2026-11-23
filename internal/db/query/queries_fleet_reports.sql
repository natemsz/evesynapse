-- Fleet battle reports, points and leaderboards (schema 170,
-- fleet_merge.go, fleet_reports.go, fleet_report_pages.go,
-- fleet_leaders.go).

-- ---------------------------------------------------------------------
-- Which fleets need building.
-- ---------------------------------------------------------------------

-- name: MarkFleetReportDirty :exec
INSERT INTO fleet_report_state (op_id, dirty_seq)
VALUES (sqlc.arg(op_id), nextval('fleet_report_seq'))
ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq;

-- Turns changed fights into marks on the fleets they feed. A fight's
-- mark is cleared only if it still holds the value read here, so a
-- store landing meanwhile keeps its own mark.
-- name: QueueFleetsForChangedFights :execrows
WITH marked AS (
    SELECT id, user_id, character_id, started_at, ended_at, merge_dirty_seq
    FROM battle_fights
    WHERE merge_dirty_seq IS NOT NULL
    ORDER BY merge_dirty_seq
    LIMIT sqlc.arg(row_limit)
), touched AS (
    SELECT DISTINCT o.id
    FROM marked m
    JOIN op_battle_consent c ON c.character_id = m.character_id AND c.user_id = m.user_id
    JOIN ops o ON o.id = c.op_id
    WHERE m.ended_at >= o.starts_at - (sqlc.arg(margin_seconds)::int * interval '1 second')
      AND m.started_at <= o.starts_at + (o.duration_minutes * interval '1 minute') + (sqlc.arg(margin_seconds)::int * interval '1 second')
), queued AS (
    INSERT INTO fleet_report_state (op_id, dirty_seq)
    SELECT t.id, nextval('fleet_report_seq') FROM touched t
    ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq
)
UPDATE battle_fights f SET merge_dirty_seq = NULL
FROM marked m
WHERE f.id = m.id AND f.merge_dirty_seq = m.merge_dirty_seq;

-- Closed fleets with attendance that have never been built, and those
-- still inside the window late killmails arrive in whose last build is
-- older than refresh_before.
-- name: QueueClosedFleets :exec
INSERT INTO fleet_report_state (op_id, dirty_seq)
SELECT q.id, nextval('fleet_report_seq')
FROM (
    SELECT o.id
    FROM ops o
    LEFT JOIN fleet_report_state s ON s.op_id = o.id
    WHERE o.cancelled_at IS NULL
      AND o.starts_at + (o.duration_minutes * interval '1 minute') < sqlc.arg(closed_before)::timestamptz
      AND EXISTS (SELECT 1 FROM op_attendance a WHERE a.op_id = o.id)
      AND (s.op_id IS NULL
           OR (s.dirty_seq IS NULL
               AND o.starts_at + (o.duration_minutes * interval '1 minute') >= sqlc.arg(late_since)::timestamptz
               AND s.computed_at < sqlc.arg(refresh_before)::timestamptz))
    ORDER BY o.starts_at DESC, o.id
    LIMIT sqlc.arg(row_limit)
) q
ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq;

-- name: ListFleetReportsToBuild :many
SELECT s.op_id, s.dirty_seq::bigint AS dirty_seq
FROM fleet_report_state s
JOIN ops o ON o.id = s.op_id
WHERE s.dirty_seq IS NOT NULL
  AND (o.cancelled_at IS NOT NULL
       OR o.starts_at + (o.duration_minutes * interval '1 minute') < sqlc.arg(closed_before)::timestamptz)
ORDER BY s.dirty_seq
LIMIT sqlc.arg(row_limit);

-- name: ClearFleetReportDirty :exec
UPDATE fleet_report_state
SET dirty_seq = CASE WHEN dirty_seq = sqlc.arg(dirty_seq)::bigint THEN NULL ELSE dirty_seq END,
    computed_at = sqlc.arg(computed_at)
WHERE op_id = sqlc.arg(op_id);

-- ---------------------------------------------------------------------
-- Consent.
-- ---------------------------------------------------------------------

-- name: SetFleetBattleConsent :exec
INSERT INTO op_battle_consent (op_id, character_id, user_id, consented_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), sqlc.arg(user_id), sqlc.arg(consented_at))
ON CONFLICT (op_id, character_id) DO UPDATE SET
    user_id = excluded.user_id, consented_at = excluded.consented_at;

-- name: DeleteFleetBattleConsent :exec
DELETE FROM op_battle_consent WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);

-- name: ListFleetBattleConsentFor :many
SELECT character_id FROM op_battle_consent
WHERE op_id = sqlc.arg(op_id) AND user_id = sqlc.arg(user_id)
  AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- Which of some characters flew this fleet.
-- name: ListOpAttendanceOf :many
SELECT character_id FROM op_attendance
WHERE op_id = sqlc.arg(op_id) AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- The names of some communities, for a board's rows.
-- name: ListCommunityLabels :many
SELECT id, name, slug FROM communities WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- ---------------------------------------------------------------------
-- What a build reads.
-- ---------------------------------------------------------------------

-- Every stored read of the consenting pilots that touches the window,
-- reaching back by the longest possible fight. aligned_at must equal
-- received_at for the read's clock steps to be usable.
-- name: ListFleetLogReads :many
SELECT f.id, f.character_id, f.started_at, f.ended_at, f.received_at, f.system_id,
       (s.aligned_at IS NOT NULL AND s.aligned_at = f.received_at)::boolean AS usable
FROM battle_fights f
JOIN op_battle_consent c ON c.op_id = sqlc.arg(op_id) AND c.character_id = f.character_id AND c.user_id = f.user_id
LEFT JOIN battle_fight_series s ON s.fight_id = f.id
WHERE f.started_at >= sqlc.arg(earliest_start)::timestamptz
  AND f.started_at <= sqlc.arg(to_at)::timestamptz
  AND f.ended_at >= sqlc.arg(from_at)::timestamptz
ORDER BY f.character_id, f.started_at, f.id
LIMIT 3000;

-- name: GetBattleFightAligned :one
SELECT aligned FROM battle_fight_series WHERE fight_id = $1;

-- The parties of a fight that could be a fleet member.
-- name: ListBattleFightPilotParties :many
SELECT seq, name, character_id FROM battle_fight_parties
WHERE fight_id = $1 AND (pilot OR character_id > 0);

-- Public mails in a window with a roster pilot on them. counts is false
-- for padding, by the same rule as FillKillLeadersWin.
-- name: ListFleetMails :many
SELECT k.killmail_id, k.killmail_time, k.system_id, k.victim_character_id, k.ship_type_id,
       k.damage_taken, k.value_ours,
       (NOT k.npc
        AND COALESCE(k.value_ours, 0) >= sqlc.arg(padding_value)::double precision
        AND NOT (k.victim_character_id > 0 AND f.own_pilot)
        AND NOT (k.victim_corporation_id >= 2000000 AND f.any_pilot AND NOT f.outside_pilot))::boolean AS counts
FROM kills k
CROSS JOIN LATERAL (
    SELECT COALESCE(bool_or(a.character_id = k.victim_character_id), false) AS own_pilot,
           COALESCE(bool_or(a.character_id > 0), false) AS any_pilot,
           COALESCE(bool_or(a.character_id > 0 AND a.corporation_id <> k.victim_corporation_id), false) AS outside_pilot,
           COALESCE(bool_or(a.character_id = ANY(sqlc.arg(roster)::bigint[])), false) AS roster_attacker
    FROM kill_attackers a WHERE a.killmail_id = k.killmail_id
) f
WHERE k.public
  AND k.killmail_time >= sqlc.arg(from_at)::timestamptz
  AND k.killmail_time <= sqlc.arg(to_at)::timestamptz
  AND (k.victim_character_id = ANY(sqlc.arg(roster)::bigint[]) OR f.roster_attacker)
ORDER BY k.killmail_time, k.killmail_id
LIMIT 5000;

-- name: ListFleetMailAttackers :many
SELECT killmail_id, character_id, ship_type_id, damage_done FROM kill_attackers
WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[])
  AND character_id = ANY(sqlc.arg(roster)::bigint[]);

-- Pilots the fleet read saw on an earlier fleet at the same time, so one
-- stretch of flying earns attendance once: once on an owner's own board,
-- and once in public. Hand marks say nothing about when a pilot flew.
-- name: ListPilotsOnEarlierOverlappingFleet :many
SELECT a.character_id,
       bool_or(o.owner_kind = me.owner_kind AND o.owner_id = me.owner_id)::boolean AS same_owner,
       bool_or(o.visibility = 'public')::boolean AS earlier_public
FROM op_attendance a
JOIN ops me ON me.id = a.op_id
JOIN op_attendance e ON e.character_id = a.character_id AND e.op_id < a.op_id AND e.source = 'fleet'
JOIN ops o ON o.id = e.op_id AND o.cancelled_at IS NULL
WHERE a.op_id = sqlc.arg(op_id) AND a.source = 'fleet'
  AND COALESCE(e.first_fleet_at, e.first_seen_at) <= a.last_seen_at
  AND e.last_seen_at >= COALESCE(a.first_fleet_at, a.first_seen_at)
GROUP BY a.character_id;

-- Marks later fleets sharing a sighting with this one, whose attendance
-- credit depends on it.
-- name: MarkLaterOverlappingFleets :exec
INSERT INTO fleet_report_state (op_id, dirty_seq)
SELECT g.op_id, nextval('fleet_report_seq')
FROM (
    SELECT DISTINCT l.op_id
    FROM op_attendance a
    JOIN op_attendance l ON l.character_id = a.character_id AND l.op_id > a.op_id AND l.source = 'fleet'
    WHERE a.op_id = sqlc.arg(op_id) AND a.source = 'fleet'
      AND COALESCE(a.first_fleet_at, a.first_seen_at) <= l.last_seen_at
      AND a.last_seen_at >= COALESCE(l.first_fleet_at, l.first_seen_at)
) g
ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq;

-- name: OpHasAttendance :one
SELECT EXISTS (SELECT 1 FROM op_attendance WHERE op_id = $1)::boolean;

-- ---------------------------------------------------------------------
-- What a build writes.
-- ---------------------------------------------------------------------

-- name: DeleteFleetBattles :exec
DELETE FROM fleet_battles WHERE op_id = $1;

-- name: DeleteFleetScorePilots :exec
DELETE FROM fleet_score_pilots WHERE op_id = $1;

-- name: DeleteFleetScoreMails :exec
DELETE FROM fleet_score_mails WHERE op_id = $1;

-- name: InsertFleetBattle :one
INSERT INTO fleet_battles (
    op_id, seq, system_id, started_at, ended_at, pilots_total, pilots_with_logs, logs_left_out,
    kills, losses, isk_destroyed, isk_lost, unpriced_mails,
    damage_out, damage_in, reps_out, cap_out, friendly_fire, computed_at
) VALUES (
    sqlc.arg(op_id), sqlc.arg(seq), sqlc.arg(system_id), sqlc.arg(started_at), sqlc.arg(ended_at),
    sqlc.arg(pilots_total), sqlc.arg(pilots_with_logs), sqlc.arg(logs_left_out),
    sqlc.arg(kills), sqlc.arg(losses), sqlc.arg(isk_destroyed), sqlc.arg(isk_lost), sqlc.arg(unpriced_mails),
    sqlc.arg(damage_out), sqlc.arg(damage_in), sqlc.arg(reps_out), sqlc.arg(cap_out), sqlc.arg(friendly_fire),
    sqlc.arg(computed_at)
)
RETURNING id;

-- Links a battle to its fights. The link holds each fight against a
-- delete, and a count short of the list means one has gone already.
-- name: InsertFleetBattleFights :execrows
INSERT INTO fleet_battle_fights (fleet_battle_id, fight_id)
SELECT sqlc.arg(fleet_battle_id)::bigint, f.id
FROM battle_fights f
WHERE f.id = ANY(sqlc.arg(fight_ids)::bigint[]);

-- name: InsertFleetBattlePilot :exec
INSERT INTO fleet_battle_pilots (
    fleet_battle_id, character_id, ship_type_id, damage_out, damage_in, reps_out, kills_on, died, has_log
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: InsertFleetBattleKill :exec
INSERT INTO fleet_battle_kills (
    fleet_battle_id, killmail_id, killmail_time, loss, victim_character_id, ship_type_id, value_isk
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertFleetBattleSeries :exec
INSERT INTO fleet_battle_series (fleet_battle_id, payload) VALUES ($1, $2);

-- name: InsertFleetScorePilot :exec
INSERT INTO fleet_score_pilots (op_id, character_id, flown_at, seen_in_fleet, member_credit, public_credit)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: InsertFleetScoreMail :exec
INSERT INTO fleet_score_mails (op_id, character_id, killmail_id, at, loss, counts, value_isk, value_points)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- ---------------------------------------------------------------------
-- The report page.
-- ---------------------------------------------------------------------

-- name: ListFleetBattles :many
SELECT * FROM fleet_battles WHERE op_id = $1 ORDER BY seq LIMIT 20;

-- name: GetFleetBattleTotals :one
SELECT COUNT(*)::bigint AS battles,
       COALESCE(SUM(kills), 0)::bigint AS kills,
       COALESCE(SUM(losses), 0)::bigint AS losses,
       COALESCE(SUM(isk_destroyed), 0)::double precision AS isk_destroyed,
       COALESCE(SUM(isk_lost), 0)::double precision AS isk_lost
FROM fleet_battles WHERE op_id = $1;

-- name: ListFleetBattlePilots :many
SELECT * FROM fleet_battle_pilots
WHERE fleet_battle_id = $1
ORDER BY damage_out DESC, reps_out DESC, character_id
LIMIT 100;

-- name: ListFleetBattleKills :many
SELECT * FROM fleet_battle_kills
WHERE fleet_battle_id = $1
ORDER BY killmail_time, killmail_id
LIMIT 100;

-- name: GetFleetBattleSeries :one
SELECT payload FROM fleet_battle_series WHERE fleet_battle_id = $1;

-- name: GetFleetReportState :one
SELECT op_id, dirty_seq, computed_at FROM fleet_report_state WHERE op_id = $1;

-- ---------------------------------------------------------------------
-- Leaderboards. A run replaces every row in one transaction.
-- ---------------------------------------------------------------------

-- name: DeleteFleetLeaders :exec
DELETE FROM fleet_leaders;

-- Ranks pilots within New Eden or within each owner. A mail counts
-- once per pilot and scope, however many of the scope's fleets it fell
-- in, and padding never scores. Boards read each fleet's current access,
-- so a change applies at the next run. A public board names only pilots
-- of fleets whose planner made names public. A members board counts only
-- fleets every member may open and read the names of.
-- name: RankFleetPilots :exec
WITH credited AS (
    SELECT CASE WHEN sqlc.arg(by_owner)::boolean THEN o.owner_kind ELSE '' END AS scope_kind,
           CASE WHEN sqlc.arg(by_owner)::boolean THEN o.owner_id ELSE 0::bigint END AS scope_id,
           s.character_id, s.op_id
    FROM fleet_score_pilots s
    JOIN ops o ON o.id = s.op_id AND o.cancelled_at IS NULL
    WHERE s.flown_at >= sqlc.arg(since)::timestamptz
      AND CASE WHEN sqlc.arg(public_only)::boolean THEN o.visibility = 'public' AND o.roster_public AND s.public_credit
               ELSE s.member_credit
                    AND (o.visibility <> 'restricted' OR NOT EXISTS (SELECT 1 FROM op_access_rules r WHERE r.op_id = o.id AND r.action = 'view'))
                    AND (o.roster_public OR o.join_gate <> 'rule' OR NOT EXISTS (SELECT 1 FROM op_access_rules r WHERE r.op_id = o.id AND r.action = 'join'))
          END
), mails AS (
    SELECT DISTINCT ON (x.scope_kind, x.scope_id, x.character_id, x.killmail_id)
           x.scope_kind, x.scope_id, x.character_id, x.killmail_id, x.loss, x.value_isk, x.value_points
    FROM (
        SELECT CASE WHEN sqlc.arg(by_owner)::boolean THEN o.owner_kind ELSE '' END AS scope_kind,
               CASE WHEN sqlc.arg(by_owner)::boolean THEN o.owner_id ELSE 0::bigint END AS scope_id,
               m.character_id, m.killmail_id, m.loss, m.value_isk, m.value_points, m.op_id
        FROM fleet_score_mails m
        JOIN fleet_score_pilots s ON s.op_id = m.op_id AND s.character_id = m.character_id
        JOIN ops o ON o.id = m.op_id AND o.cancelled_at IS NULL
        WHERE m.counts AND m.at >= sqlc.arg(since)::timestamptz
          AND CASE WHEN sqlc.arg(public_only)::boolean THEN o.visibility = 'public' AND o.roster_public AND s.seen_in_fleet
                   ELSE (o.visibility <> 'restricted' OR NOT EXISTS (SELECT 1 FROM op_access_rules r WHERE r.op_id = o.id AND r.action = 'view'))
                        AND (o.roster_public OR o.join_gate <> 'rule' OR NOT EXISTS (SELECT 1 FROM op_access_rules r WHERE r.op_id = o.id AND r.action = 'join'))
              END
    ) x
    ORDER BY x.scope_kind, x.scope_id, x.character_id, x.killmail_id, x.op_id
), fleet_totals AS (
    SELECT scope_kind, scope_id, character_id, COUNT(DISTINCT op_id)::bigint AS fleets
    FROM credited GROUP BY scope_kind, scope_id, character_id
), mail_totals AS (
    SELECT scope_kind, scope_id, character_id,
           COUNT(*) FILTER (WHERE NOT loss)::bigint AS kills,
           COUNT(*) FILTER (WHERE loss)::bigint AS deaths,
           COALESCE(SUM(value_isk) FILTER (WHERE NOT loss), 0)::double precision AS isk,
           COALESCE(SUM(value_points) FILTER (WHERE NOT loss), 0)::double precision AS value_points
    FROM mails GROUP BY scope_kind, scope_id, character_id
), joined AS (
    SELECT COALESCE(f.scope_kind, m.scope_kind) AS scope_kind,
           COALESCE(f.scope_id, m.scope_id) AS scope_id,
           COALESCE(f.character_id, m.character_id) AS character_id,
           COALESCE(f.fleets, 0)::bigint AS fleets,
           COALESCE(m.kills, 0)::bigint AS kills,
           COALESCE(m.deaths, 0)::bigint AS deaths,
           COALESCE(m.isk, 0)::double precision AS isk,
           GREATEST(0, COALESCE(f.fleets, 0) * sqlc.arg(points_fleet)::bigint
                     + COALESCE(m.kills, 0) * sqlc.arg(points_kill)::bigint
                     - COALESCE(m.deaths, 0) * sqlc.arg(points_death)::bigint
                     + ROUND(COALESCE(m.value_points, 0))::bigint)::bigint AS points
    FROM fleet_totals f
    FULL JOIN mail_totals m USING (scope_kind, scope_id, character_id)
)
INSERT INTO fleet_leaders (period, audience, scope_kind, scope_id, board, entity_kind, entity_id, place, fleets, kills, deaths, isk, points)
SELECT sqlc.arg(period)::text,
       CASE WHEN sqlc.arg(public_only)::boolean THEN 'public' ELSE 'members' END,
       j.scope_kind, j.scope_id, 'pilot', 'character', j.character_id,
       row_number() OVER (PARTITION BY j.scope_kind, j.scope_id ORDER BY j.points DESC, j.kills DESC, j.isk DESC, j.character_id)::integer,
       j.fleets, j.kills, j.deaths, j.isk, j.points
FROM joined j;

-- Ranks owners across New Eden on all their public fleets, names shown
-- or not, since a group's totals name nobody. Kills, deaths and ISK
-- count each mail once per owner. Points add up what each of the owner's
-- pilots earned. A group with kills but no fleet long or large enough
-- for attendance still ranks. A community takes part only while it is
-- listed and open.
-- name: RankFleetGroups :exec
WITH credited AS (
    SELECT o.owner_kind, o.owner_id, s.character_id, s.op_id
    FROM fleet_score_pilots s
    JOIN ops o ON o.id = s.op_id AND o.cancelled_at IS NULL AND o.visibility = 'public'
    WHERE s.public_credit AND s.flown_at >= sqlc.arg(since)::timestamptz
), fleets AS (
    SELECT owner_kind, owner_id, COUNT(DISTINCT op_id)::bigint AS fleets
    FROM credited GROUP BY owner_kind, owner_id
), public_mails AS (
    SELECT o.owner_kind, o.owner_id, m.character_id, m.killmail_id, m.loss, m.value_isk, m.value_points, m.op_id
    FROM fleet_score_mails m
    JOIN fleet_score_pilots s ON s.op_id = m.op_id AND s.character_id = m.character_id AND s.seen_in_fleet
    JOIN ops o ON o.id = m.op_id AND o.cancelled_at IS NULL AND o.visibility = 'public'
    WHERE m.counts AND m.at >= sqlc.arg(since)::timestamptz
), distinct_mails AS (
    SELECT DISTINCT ON (p.owner_kind, p.owner_id, p.killmail_id)
           p.owner_kind, p.owner_id, p.killmail_id, p.loss, p.value_isk
    FROM public_mails p
    ORDER BY p.owner_kind, p.owner_id, p.killmail_id, p.loss, p.op_id
), mail_totals AS (
    SELECT owner_kind, owner_id,
           COUNT(*) FILTER (WHERE NOT loss)::bigint AS kills,
           COUNT(*) FILTER (WHERE loss)::bigint AS deaths,
           COALESCE(SUM(value_isk) FILTER (WHERE NOT loss), 0)::double precision AS isk
    FROM distinct_mails GROUP BY owner_kind, owner_id
), pilot_mails AS (
    SELECT DISTINCT ON (p.owner_kind, p.owner_id, p.character_id, p.killmail_id)
           p.owner_kind, p.owner_id, p.character_id, p.killmail_id, p.loss, p.value_points
    FROM public_mails p
    ORDER BY p.owner_kind, p.owner_id, p.character_id, p.killmail_id, p.op_id
), pilot_mail_totals AS (
    SELECT owner_kind, owner_id, character_id,
           COUNT(*) FILTER (WHERE NOT loss)::bigint AS kills,
           COUNT(*) FILTER (WHERE loss)::bigint AS deaths,
           COALESCE(SUM(value_points) FILTER (WHERE NOT loss), 0)::double precision AS value_points
    FROM pilot_mails GROUP BY owner_kind, owner_id, character_id
), pilot_fleets AS (
    SELECT owner_kind, owner_id, character_id, COUNT(DISTINCT op_id)::bigint AS fleets
    FROM credited GROUP BY owner_kind, owner_id, character_id
), points AS (
    SELECT e.owner_kind, e.owner_id,
           SUM(GREATEST(0, e.fleets * sqlc.arg(points_fleet)::bigint
                         + e.kills * sqlc.arg(points_kill)::bigint
                         - e.deaths * sqlc.arg(points_death)::bigint
                         + ROUND(e.value_points)::bigint))::bigint AS points
    FROM (
        SELECT COALESCE(f.owner_kind, m.owner_kind) AS owner_kind,
               COALESCE(f.owner_id, m.owner_id) AS owner_id,
               COALESCE(f.fleets, 0)::bigint AS fleets,
               COALESCE(m.kills, 0)::bigint AS kills,
               COALESCE(m.deaths, 0)::bigint AS deaths,
               COALESCE(m.value_points, 0)::double precision AS value_points
        FROM pilot_fleets f
        FULL JOIN pilot_mail_totals m
          ON m.owner_kind = f.owner_kind AND m.owner_id = f.owner_id AND m.character_id = f.character_id
    ) e
    GROUP BY e.owner_kind, e.owner_id
), joined AS (
    SELECT COALESCE(f.owner_kind, m.owner_kind) AS owner_kind,
           COALESCE(f.owner_id, m.owner_id) AS owner_id,
           COALESCE(f.fleets, 0)::bigint AS fleets,
           COALESCE(m.kills, 0)::bigint AS kills,
           COALESCE(m.deaths, 0)::bigint AS deaths,
           COALESCE(m.isk, 0)::double precision AS isk,
           COALESCE(p.points, 0)::bigint AS points
    FROM fleets f
    FULL JOIN mail_totals m ON m.owner_kind = f.owner_kind AND m.owner_id = f.owner_id
    LEFT JOIN points p ON p.owner_kind = COALESCE(f.owner_kind, m.owner_kind) AND p.owner_id = COALESCE(f.owner_id, m.owner_id)
    LEFT JOIN communities c ON COALESCE(f.owner_kind, m.owner_kind) = 'community' AND c.id = COALESCE(f.owner_id, m.owner_id)
    WHERE COALESCE(f.owner_kind, m.owner_kind) <> 'community' OR (c.listed AND c.archived_at IS NULL)
)
INSERT INTO fleet_leaders (period, audience, scope_kind, scope_id, board, entity_kind, entity_id, place, fleets, kills, deaths, isk, points)
SELECT sqlc.arg(period)::text, 'public', '', 0::bigint, 'group', j.owner_kind, j.owner_id,
       row_number() OVER (ORDER BY j.points DESC, j.kills DESC, j.isk DESC, j.owner_kind, j.owner_id)::integer,
       j.fleets, j.kills, j.deaths, j.isk, j.points
FROM joined j;

-- One board, best first. A community that has since been unlisted or
-- closed drops out at once, before the next run.
-- name: ListFleetLeaders :many
SELECT l.entity_kind, l.entity_id, l.place, l.fleets, l.kills, l.deaths, l.isk, l.points
FROM fleet_leaders l
LEFT JOIN communities c ON l.entity_kind = 'community' AND c.id = l.entity_id
WHERE l.period = sqlc.arg(period)::text AND l.audience = sqlc.arg(audience)::text
  AND l.scope_kind = sqlc.arg(scope_kind)::text AND l.scope_id = sqlc.arg(scope_id)::bigint
  AND l.board = sqlc.arg(board)::text
  AND (l.entity_kind <> 'community' OR (c.listed AND c.archived_at IS NULL))
ORDER BY l.place
LIMIT sqlc.arg(row_limit);

-- Whether an owner is on a public board in any period: with named
-- pilots of its own, or as a group.
-- name: FleetBoardExists :one
SELECT (EXISTS (
    SELECT 1 FROM fleet_leaders
    WHERE period = ANY(sqlc.arg(periods)::text[]) AND audience = 'public'
      AND scope_kind = sqlc.arg(scope_kind)::text AND scope_id = sqlc.arg(scope_id)::bigint
      AND board = 'pilot'
) OR EXISTS (
    SELECT 1 FROM fleet_leaders
    WHERE period = ANY(sqlc.arg(periods)::text[]) AND audience = 'public'
      AND scope_kind = '' AND scope_id = 0 AND board = 'group'
      AND entity_kind = sqlc.arg(scope_kind)::text AND entity_id = sqlc.arg(scope_id)::bigint
))::boolean;

-- Where one pilot or group stands on the New Eden public board.
-- name: GetFleetLeaderPlace :one
SELECT place, fleets, kills, deaths, isk, points
FROM fleet_leaders
WHERE period = sqlc.arg(period)::text AND audience = 'public' AND scope_kind = '' AND scope_id = 0
  AND board = sqlc.arg(board)::text AND entity_kind = sqlc.arg(entity_kind)::text AND entity_id = sqlc.arg(entity_id)::bigint;
