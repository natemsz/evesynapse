-- Anonymous fit sharing (schema 067): link-only fits under a
-- shared anonymous owner, remembered by a signed cookie.

-- name: SetAnonymousFittingShare :exec
UPDATE local_fittings
SET share_token = $2, is_anonymous = true, last_viewed_at = now()
WHERE id = $1;

-- name: GetFittingByShareToken :one
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       lf.view_count, lf.total_price, lf.rating_count, lf.rating_sum,
       ''::text AS author_name
FROM local_fittings lf
WHERE lf.share_token = $1 AND lf.is_anonymous = true AND NOT lf.is_draft;

-- name: ListFittingsByShareTokens :many
SELECT id, user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at, share_token, is_anonymous
FROM local_fittings
WHERE share_token = ANY(sqlc.arg(tokens)::text[]) AND is_anonymous = true;

-- name: DeleteAnonymousFittingByToken :execrows
DELETE FROM local_fittings
WHERE share_token = $1 AND is_anonymous = true;

-- name: TouchFittingByToken :exec
UPDATE local_fittings
SET last_viewed_at = now()
WHERE share_token = $1 AND is_anonymous = true;

-- name: DeleteExpiredAnonymousFittings :execrows
DELETE FROM local_fittings
WHERE is_anonymous = true AND last_viewed_at < $1;

-- name: GetFittingAnonymousOwner :one
SELECT user_id FROM fitting_anonymous_owner WHERE id = 1;

-- name: InsertFittingAnonymousOwner :exec
INSERT INTO fitting_anonymous_owner (id, user_id)
VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;

-- name: GetFittingCookieSecret :one
SELECT secret FROM fitting_cookie_secrets WHERE id = 1;

-- name: InsertFittingCookieSecret :exec
INSERT INTO fitting_cookie_secrets (id, secret)
VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;
