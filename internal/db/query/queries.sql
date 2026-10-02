-- name: CreateUser :one
INSERT INTO users (created_at)
VALUES (datetime('now'))
RETURNING *;

-- name: GetUser :one
SELECT * FROM users
WHERE id = ?;

-- name: UpsertCharacter :one
INSERT INTO characters (
    character_id, user_id, name,
    access_token, refresh_token, token_expiry,
    scopes, cached_until, updated_at
) VALUES (
    ?, ?, ?,
    ?, ?, ?,
    ?, ?, datetime('now')
)
ON CONFLICT (character_id) DO UPDATE SET
    user_id       = excluded.user_id,
    name          = excluded.name,
    access_token  = excluded.access_token,
    refresh_token = excluded.refresh_token,
    token_expiry  = excluded.token_expiry,
    scopes        = excluded.scopes,
    cached_until  = excluded.cached_until,
    updated_at    = excluded.updated_at
RETURNING *;

-- name: GetCharacter :one
SELECT * FROM characters
WHERE character_id = ?;

-- name: ListCharactersByUser :many
SELECT * FROM characters
WHERE user_id = ?
ORDER BY name;

-- name: DeleteCharacter :exec
DELETE FROM characters
WHERE character_id = ? AND user_id = ?;

-- name: ListUsers :many
SELECT * FROM users
ORDER BY id;

-- name: ListAllCharacters :many
SELECT * FROM characters
ORDER BY user_id, name;
