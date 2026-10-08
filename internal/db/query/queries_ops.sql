-- ---------------------------------------------------------------------
-- Ops (schema 014): EveSynapse's own calendar entries and the
-- sign-ups to them.
-- ---------------------------------------------------------------------

-- The corporation of each of an account's characters.
-- name: ListCharacterCorporationsByUser :many
SELECT c.character_id, c.name, cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE c.user_id = $1
ORDER BY c.character_id;

-- name: CreateOp :one
INSERT INTO ops (corporation_id, title, description, starts_at, duration_minutes, doctrine, form_up, fc_character_id, created_by_character, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id;

-- name: UpdateOp :exec
UPDATE ops SET
    title            = sqlc.arg(title),
    description      = sqlc.arg(description),
    starts_at        = sqlc.arg(starts_at),
    duration_minutes = sqlc.arg(duration_minutes),
    doctrine         = sqlc.arg(doctrine),
    form_up          = sqlc.arg(form_up),
    fc_character_id  = sqlc.arg(fc_character_id)
WHERE id = sqlc.arg(id);

-- name: GetOp :one
SELECT * FROM ops WHERE id = $1;

-- name: SetOpCancelled :exec
UPDATE ops SET cancelled_at = sqlc.arg(cancelled_at) WHERE id = sqlc.arg(id);

-- name: ListOpsForCorporationsBetween :many
SELECT * FROM ops
WHERE corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[])
  AND starts_at >= sqlc.arg(from_time) AND starts_at < sqlc.arg(to_time)
ORDER BY starts_at, id;

-- name: UpsertOpSignup :exec
INSERT INTO op_signups (op_id, character_id, user_id, response, ship, fleet_role, note, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (op_id, character_id) DO UPDATE SET
    user_id    = excluded.user_id,
    response   = excluded.response,
    ship       = excluded.ship,
    fleet_role = excluded.fleet_role,
    note       = excluded.note,
    updated_at = excluded.updated_at;

-- name: ListOpSignups :many
SELECT * FROM op_signups WHERE op_id = $1 ORDER BY character_id;

-- name: ListOpSignupsForOps :many
SELECT op_id, character_id, user_id, response FROM op_signups
WHERE op_id = ANY(sqlc.arg(op_ids)::bigint[]);

-- ---------------------------------------------------------------------
-- Attendance (schema 015).
-- ---------------------------------------------------------------------

-- Ops whose fleet may be up right now: not cancelled, and inside the
-- window from a little before the start to a little after the end.
-- name: ListOpsToCapture :many
SELECT * FROM ops
WHERE cancelled_at IS NULL
  AND fc_character_id <> 0
  AND starts_at <= sqlc.arg(starts_before)
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY starts_at, id;

-- name: SetOpCaptureStatus :exec
UPDATE ops SET capture_status = sqlc.arg(capture_status), capture_checked_at = sqlc.arg(checked_at)
WHERE id = sqlc.arg(id);

-- Seen in the fleet. A character already marked by hand becomes a
-- fleet sighting: the fleet is the better witness.
-- name: UpsertFleetAttendance :exec
INSERT INTO op_attendance (op_id, character_id, ship_type_id, source, first_seen_at, last_seen_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), sqlc.arg(ship_type_id), 'fleet', sqlc.arg(seen_at), sqlc.arg(seen_at))
ON CONFLICT (op_id, character_id) DO UPDATE SET
    ship_type_id = excluded.ship_type_id,
    source       = 'fleet',
    last_seen_at = excluded.last_seen_at;

-- Marked by hand. Never touches a row the fleet reported.
-- name: InsertManualAttendance :exec
INSERT INTO op_attendance (op_id, character_id, source, first_seen_at, last_seen_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), 'manual', sqlc.arg(seen_at), sqlc.arg(seen_at))
ON CONFLICT (op_id, character_id) DO NOTHING;

-- name: DeleteManualAttendanceExcept :exec
DELETE FROM op_attendance
WHERE op_id = sqlc.arg(op_id) AND source = 'manual'
  AND NOT (character_id = ANY(sqlc.arg(keep_ids)::bigint[]));

-- name: ListOpAttendance :many
SELECT * FROM op_attendance WHERE op_id = $1 ORDER BY character_id;

-- Attendance counts for a corporation's ops since a date, per
-- character: the PAP table.
-- name: ListCorporationPAPs :many
SELECT a.character_id,
       COUNT(*)::bigint AS total,
       COUNT(*) FILTER (WHERE o.starts_at >= sqlc.arg(recent_since))::bigint AS recent,
       MAX(o.starts_at)::timestamptz AS last_op
FROM op_attendance a
JOIN ops o ON o.id = a.op_id
WHERE o.corporation_id = sqlc.arg(corporation_id)
  AND o.cancelled_at IS NULL
  AND o.starts_at >= sqlc.arg(since)
GROUP BY a.character_id
ORDER BY total DESC, a.character_id;
