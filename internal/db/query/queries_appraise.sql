-- Appraisal tool storage: a signed-in user's saved appraisals
-- (schema 058), anonymous appraisals under the shared anonymous
-- owner (schema 064), and the case-insensitive name lookup the
-- paste parser resolves against.

-- name: InsertUserAppraisal :one
INSERT INTO user_appraisals (user_id, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now())
RETURNING id;

-- name: ListUserAppraisals :many
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at
FROM user_appraisals
WHERE user_id = $1
ORDER BY id DESC
LIMIT $2;

-- name: GetUserAppraisal :one
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at
FROM user_appraisals
WHERE id = $1 AND user_id = $2;

-- name: GetUserAppraisalByShareToken :one
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at
FROM user_appraisals
WHERE share_token = $1;

-- name: ListUserAppraisalsByShareTokens :many
SELECT id, user_id, created_at, input_text, input_hash, share_token, snapshot, total_value, priced_lines, total_lines, is_anonymous, last_viewed_at
FROM user_appraisals
WHERE share_token = ANY(sqlc.arg(tokens)::text[]);

-- name: DeleteUserAppraisal :execrows
DELETE FROM user_appraisals
WHERE id = $1 AND user_id = $2;

-- name: DeleteAnonymousAppraisalByToken :execrows
DELETE FROM user_appraisals
WHERE share_token = $1 AND is_anonymous = true;

-- name: TouchUserAppraisalByToken :exec
UPDATE user_appraisals
SET last_viewed_at = now()
WHERE share_token = $1 AND is_anonymous = true;

-- name: DeleteExpiredAnonymousAppraisals :execrows
DELETE FROM user_appraisals
WHERE is_anonymous = true AND last_viewed_at < $1;

-- name: GetAppraisalAnonymousOwner :one
SELECT user_id FROM appraisal_anonymous_owner WHERE id = 1;

-- name: InsertAppraisalAnonymousOwner :exec
INSERT INTO appraisal_anonymous_owner (id, user_id)
VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;

-- name: GetAppraisalCookieSecret :one
SELECT secret FROM appraisal_cookie_secrets WHERE id = 1;

-- name: InsertAppraisalCookieSecret :exec
INSERT INTO appraisal_cookie_secrets (id, secret)
VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;

-- name: GetUserAppraisalPref :one
SELECT user_id, hub_region, mode, percent, updated_at FROM user_appraisal_prefs
WHERE user_id = $1;

-- name: UpsertUserAppraisalPref :exec
INSERT INTO user_appraisal_prefs (user_id, hub_region, mode, percent, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id) DO UPDATE SET
    hub_region = excluded.hub_region,
    mode       = excluded.mode,
    percent    = excluded.percent,
    updated_at = excluded.updated_at;

-- Pasted names arrive in whatever case the player typed; the
-- SDE's canonical case comes back with the row.
-- name: ListSDETypesByLowerNames :many
SELECT type_id, name FROM sde_types
WHERE lower(name) = ANY(sqlc.arg(names)::text[]);
