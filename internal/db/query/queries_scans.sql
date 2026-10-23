-- ---------------------------------------------------------------------
-- Scans (schema 077): pasted d-scans, local lists, fleet compositions
-- and probe results, unlisted, kept 90 days.
-- ---------------------------------------------------------------------

-- name: InsertScan :exec
INSERT INTO scans (id, kind, system_id, user_id, group_id, raw, result, created_at)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(system_id), sqlc.arg(user_id), sqlc.arg(group_id), sqlc.arg(raw), sqlc.arg(result), sqlc.arg(created_at));

-- name: GetScan :one
SELECT id, kind, system_id, user_id, group_id, raw, result, created_at FROM scans WHERE id = $1;

-- A scan group (schema 086): every scan filed under one group
-- id, oldest first -- the group's timeline on a member's page.
-- name: ListScanGroup :many
SELECT id, kind, system_id, user_id, created_at, result FROM scans
WHERE group_id = sqlc.arg(group_id) OR id = sqlc.arg(group_id)
ORDER BY created_at ASC
LIMIT sqlc.arg(max_rows)::int;

-- Recent scans of one system, newest first, leaving out one
-- scan and, when it is set, every scan of its group: the
-- reading page's related-scans rail.
-- name: ListRelatedScans :many
SELECT id, kind, system_id, user_id, group_id, created_at, result FROM scans
WHERE system_id = sqlc.arg(system_id)::bigint
  AND id <> sqlc.arg(exclude_id)
  AND (sqlc.arg(exclude_group)::text = '' OR (group_id <> sqlc.arg(exclude_group) AND id <> sqlc.arg(exclude_group)))
  AND created_at > sqlc.arg(since)::timestamptz
ORDER BY created_at DESC
LIMIT sqlc.arg(max_rows)::int;

-- A reading's system, corrected after the paste: the column
-- and the stored reading move together.
-- name: UpdateScanSystem :exec
UPDATE scans SET system_id = sqlc.arg(system_id), result = sqlc.arg(result) WHERE id = sqlc.arg(id);

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
