-- ---------------------------------------------------------------------
-- Scans (schema 077): pasted d-scans, local lists, fleet compositions
-- and probe results, unlisted, kept 90 days.
-- ---------------------------------------------------------------------

-- name: InsertScan :exec
INSERT INTO scans (id, kind, system_id, user_id, raw, result, created_at)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(system_id), sqlc.arg(user_id), sqlc.arg(raw), sqlc.arg(result), sqlc.arg(created_at));

-- name: GetScan :one
SELECT id, kind, system_id, user_id, raw, result, created_at FROM scans WHERE id = $1;

-- A reader's own scans, newest first: those they pasted signed in,
-- and those this session pasted (by id).
-- name: ListOwnScans :many
SELECT id, kind, system_id, created_at FROM scans
WHERE (sqlc.arg(user_id)::bigint > 0 AND user_id = sqlc.arg(user_id)::bigint)
   OR id = ANY(sqlc.arg(ids)::text[])
ORDER BY created_at DESC
LIMIT sqlc.arg(max_rows)::int;

-- name: PruneScans :execrows
DELETE FROM scans WHERE created_at < sqlc.arg(before)::timestamptz;

-- Recent public PvP by each of some characters: kills they were on
-- in the window, the latest, and where. The local scan's "known
-- killers" column; only posted (public) kills count.
-- name: ListPilotKillActivity :many
SELECT a.character_id,
       COUNT(DISTINCT a.killmail_id)::bigint AS kills,
       MAX(k.killmail_time)::timestamptz AS last_kill,
       (ARRAY_AGG(k.system_id ORDER BY k.killmail_time DESC))[1]::bigint AS last_system
FROM kill_attackers a
JOIN kills k ON k.killmail_id = a.killmail_id
WHERE a.character_id = ANY(sqlc.arg(character_ids)::bigint[])
  AND k.public AND NOT k.npc
  AND k.killmail_time >= sqlc.arg(since)::timestamptz
GROUP BY a.character_id;
