-- Appraisal tool storage: a signed-in user's saved appraisals
-- (schema 058) and the case-insensitive name lookup the paste
-- parser resolves against.

-- name: InsertUserAppraisal :one
INSERT INTO user_appraisals (user_id, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: ListUserAppraisals :many
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines
FROM user_appraisals
WHERE user_id = $1
ORDER BY id DESC
LIMIT $2;

-- name: GetUserAppraisal :one
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines
FROM user_appraisals
WHERE id = $1 AND user_id = $2;

-- name: GetUserAppraisalByShareToken :one
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines
FROM user_appraisals
WHERE share_token = $1;

-- Pasted names arrive in whatever case the player typed; the
-- SDE's canonical case comes back with the row.
-- name: ListSDETypesByLowerNames :many
SELECT type_id, name FROM sde_types
WHERE lower(name) = ANY(sqlc.arg(names)::text[]);
