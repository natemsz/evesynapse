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
