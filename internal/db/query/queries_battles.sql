-- Battle reports (schema 124, battle_ingest.go, battle_correlate.go,
-- battle_pages.go).

-- name: UpsertBattleFight :one
-- A fight posted again (Axon restarted, a retry) replaces its frame
-- and totals; its share link and sharing choice stay.
INSERT INTO battle_fights (
    user_id, character_id, listener, session_file, started_at, ended_at,
    damage_out, damage_in, reps_out, reps_in, cap_in, peak_in_dps, peak_in_at,
    pilots, lines, unparsed, share_token, received_at
) VALUES (
    sqlc.arg(user_id), sqlc.arg(character_id), sqlc.arg(listener), sqlc.arg(session_file),
    sqlc.arg(started_at), sqlc.arg(ended_at),
    sqlc.arg(damage_out), sqlc.arg(damage_in), sqlc.arg(reps_out), sqlc.arg(reps_in),
    sqlc.arg(cap_in), sqlc.arg(peak_in_dps), sqlc.arg(peak_in_at),
    sqlc.arg(pilots), sqlc.arg(lines), sqlc.arg(unparsed), sqlc.arg(share_token), sqlc.arg(received_at)
)
ON CONFLICT (character_id, started_at) DO UPDATE SET
    user_id      = excluded.user_id,
    listener     = excluded.listener,
    session_file = excluded.session_file,
    ended_at     = excluded.ended_at,
    damage_out   = excluded.damage_out,
    damage_in    = excluded.damage_in,
    reps_out     = excluded.reps_out,
    reps_in      = excluded.reps_in,
    cap_in       = excluded.cap_in,
    peak_in_dps  = excluded.peak_in_dps,
    peak_in_at   = excluded.peak_in_at,
    pilots       = excluded.pilots,
    lines        = excluded.lines,
    unparsed     = excluded.unparsed,
    received_at  = excluded.received_at,
    correlated_at = NULL
RETURNING id, (xmax = 0)::boolean AS inserted;

-- name: UpsertBattleFightSeries :exec
INSERT INTO battle_fight_series (fight_id, series, slices, chart)
VALUES ($1, $2, $3, '')
ON CONFLICT (fight_id) DO UPDATE SET series = excluded.series, slices = excluded.slices, chart = '';

-- name: DeleteBattleFightMoments :exec
DELETE FROM battle_fight_moments WHERE fight_id = $1;

-- name: InsertBattleFightMoment :exec
INSERT INTO battle_fight_moments (fight_id, seq, kind, at_second, end_second, title, brief, detail)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListBattleFightMoments :many
SELECT fight_id, seq, kind, at_second, end_second, title, brief FROM battle_fight_moments
WHERE fight_id = $1
ORDER BY at_second, seq
LIMIT 120;

-- name: GetBattleFightMoment :one
SELECT * FROM battle_fight_moments WHERE fight_id = $1 AND seq = $2;

-- name: SetBattleFightChart :exec
UPDATE battle_fight_series SET chart = $2 WHERE fight_id = $1;

-- name: DeleteBattleFightParties :exec
DELETE FROM battle_fight_parties WHERE fight_id = $1;

-- name: DeleteBattleFightWeapons :exec
DELETE FROM battle_fight_weapons WHERE fight_id = $1;

-- name: InsertBattleFightParty :exec
INSERT INTO battle_fight_parties (
    fight_id, seq, name, ticker, hull, hull_type_id, pilot,
    damage_out, damage_in, reps_out, reps_in, cap_out, cap_in,
    first_at, last_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
);

-- name: DeleteBattleFightTackles :exec
DELETE FROM battle_fight_tackles WHERE fight_id = $1;

-- name: InsertBattleFightTackle :exec
INSERT INTO battle_fight_tackles (fight_id, seq, from_name, to_name, kind, first_at, attempts)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListBattleFightTackles :many
SELECT * FROM battle_fight_tackles
WHERE fight_id = $1
ORDER BY first_at, seq
LIMIT 200;

-- name: ListBattleTypeCategories :many
-- Names the log used, as SDE types with their category: a ship (6),
-- a charge (8), a drone (18) or a fighter (87) tell a pilot's hull
-- from the things it launches.
SELECT t.type_id, t.name, g.category_id
FROM sde_types t JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.name = ANY(sqlc.arg(names)::text[]);

-- name: InsertBattleFightWeapon :exec
INSERT INTO battle_fight_weapons (
    fight_id, party_seq, dir, weapon, weapon_type_id, shots, damage,
    q_miss, q_graze, q_glance, q_hit, q_penetrate, q_smash, q_wreck
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
)
ON CONFLICT (fight_id, party_seq, dir, weapon) DO UPDATE SET
    shots = battle_fight_weapons.shots + excluded.shots,
    damage = battle_fight_weapons.damage + excluded.damage,
    q_miss = battle_fight_weapons.q_miss + excluded.q_miss,
    q_graze = battle_fight_weapons.q_graze + excluded.q_graze,
    q_glance = battle_fight_weapons.q_glance + excluded.q_glance,
    q_hit = battle_fight_weapons.q_hit + excluded.q_hit,
    q_penetrate = battle_fight_weapons.q_penetrate + excluded.q_penetrate,
    q_smash = battle_fight_weapons.q_smash + excluded.q_smash,
    q_wreck = battle_fight_weapons.q_wreck + excluded.q_wreck;

-- name: GetBattleFight :one
SELECT * FROM battle_fights WHERE id = $1;

-- name: GetBattleFightByToken :one
SELECT * FROM battle_fights WHERE share_token = $1 AND shared;

-- name: SetBattleFightShared :exec
UPDATE battle_fights SET shared = sqlc.arg(shared)
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: ListBattleFightsByUser :many
SELECT * FROM battle_fights
WHERE user_id = $1
ORDER BY started_at DESC
LIMIT 100;

-- name: GetBattleFightSeries :one
SELECT * FROM battle_fight_series WHERE fight_id = $1;

-- name: ListBattleFightParties :many
SELECT * FROM battle_fight_parties
WHERE fight_id = $1
ORDER BY (damage_out + damage_in + reps_out + reps_in) DESC, seq
LIMIT 200;

-- name: ListBattleFightWeapons :many
SELECT * FROM battle_fight_weapons
WHERE fight_id = $1
ORDER BY dir DESC, damage DESC
LIMIT 300;

-- name: ListBattleFightKills :many
SELECT * FROM battle_fight_kills
WHERE fight_id = $1
ORDER BY killmail_time, killmail_id
LIMIT 200;

-- name: ListBattleFightFindings :many
SELECT * FROM battle_fight_findings
WHERE fight_id = $1
ORDER BY seq
LIMIT 50;

-- name: ListBattleFightsToCorrelate :many
-- Fights still inside the window late mails arrive in, the ones
-- correlated longest ago (or never) first.
SELECT * FROM battle_fights
WHERE ended_at > sqlc.arg(since)
ORDER BY correlated_at NULLS FIRST, id
LIMIT sqlc.arg(row_limit);

-- name: ListKillsForFight :many
-- The mails a character is on, as victim or attacker, in a window,
-- with the character's own damage on each.
SELECT k.killmail_id, k.killmail_time, k.system_id, k.victim_character_id,
       k.ship_type_id, k.damage_taken, k.value_ours,
       COALESCE((SELECT a.damage_done FROM kill_attackers a
                 WHERE a.killmail_id = k.killmail_id AND a.character_id = sqlc.arg(character_id)
                 LIMIT 1), 0)::bigint AS my_damage
FROM kills k
WHERE k.killmail_time BETWEEN sqlc.arg(from_at) AND sqlc.arg(to_at)
  AND (k.victim_character_id = sqlc.arg(character_id)
       OR k.killmail_id IN (SELECT a.killmail_id FROM kill_attackers a WHERE a.character_id = sqlc.arg(character_id)))
ORDER BY k.killmail_time, k.killmail_id
LIMIT 200;

-- name: ListKillAttackerCharacters :many
SELECT killmail_id, character_id, ship_type_id, damage_done FROM kill_attackers
WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]) AND character_id > 0
ORDER BY killmail_id, seq
LIMIT 2000;

-- name: DeleteBattleFightKills :exec
DELETE FROM battle_fight_kills WHERE fight_id = $1;

-- name: InsertBattleFightKill :exec
INSERT INTO battle_fight_kills (
    fight_id, killmail_id, killmail_time, at_second, loss, victim_character_id,
    victim_name, ship_type_id, damage_taken, my_damage, ttk_seconds, value_isk
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
);

-- name: SetBattleFightPartyMatch :exec
UPDATE battle_fight_parties
SET character_id = sqlc.arg(character_id), died = sqlc.arg(died),
    hull_type_id = CASE WHEN hull_type_id = 0 THEN sqlc.arg(hull_type_id) ELSE hull_type_id END
WHERE fight_id = sqlc.arg(fight_id) AND seq = sqlc.arg(seq);

-- name: DeleteBattleFightFindings :exec
DELETE FROM battle_fight_findings WHERE fight_id = $1;

-- name: InsertBattleFightFinding :exec
INSERT INTO battle_fight_findings (fight_id, seq, rule, text) VALUES ($1, $2, $3, $4);

-- name: SetBattleFightCorrelation :exec
UPDATE battle_fights SET
    system_id = sqlc.arg(system_id),
    system_from = sqlc.arg(system_from),
    hull_type_id = sqlc.arg(hull_type_id),
    hull_from = sqlc.arg(hull_from),
    kills = sqlc.arg(kills),
    losses = sqlc.arg(losses),
    isk_destroyed = sqlc.arg(isk_destroyed),
    isk_lost = sqlc.arg(isk_lost),
    outcome = sqlc.arg(outcome),
    spark_out = sqlc.arg(spark_out),
    spark_in = sqlc.arg(spark_in),
    correlated_at = sqlc.arg(correlated_at)
WHERE id = sqlc.arg(id);

-- name: ListBattleFightIDsForKills :many
-- The viewer's own fights that matched any of these mails, for the
-- killmails page's Battle report links.
SELECT bk.killmail_id, MIN(bk.fight_id)::bigint AS fight_id
FROM battle_fight_kills bk
JOIN battle_fights f ON f.id = bk.fight_id
WHERE f.user_id = sqlc.arg(user_id) AND bk.killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[])
GROUP BY bk.killmail_id;
