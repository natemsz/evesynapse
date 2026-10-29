-- ---------------------------------------------------------------------
-- Global notices (schema 107, internal/app/notices.go). Administrators
-- write; every page reads the live ones through a short cache.
-- ---------------------------------------------------------------------

-- The banners every page shows: live now, bounded, most severe first.
-- name: ListLiveGlobalNotices :many
SELECT id, title, severity, body_html, starts_at, ends_at
FROM global_notices
WHERE retired_at IS NULL AND starts_at <= sqlc.arg(now) AND ends_at > sqlc.arg(now)
ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 WHEN 'promo' THEN 2 ELSE 3 END, id DESC
LIMIT sqlc.arg(row_limit);

-- name: CountLiveGlobalNotices :one
SELECT COUNT(*)::bigint FROM global_notices
WHERE retired_at IS NULL AND ends_at > sqlc.arg(now);

-- The admin tab's list: newest first, live and past alike.
-- name: ListRecentGlobalNotices :many
SELECT id, title, severity, body_md, urgent, urgent_sent_at, starts_at, ends_at, retired_at, created_by, created_at, updated_at
FROM global_notices
ORDER BY id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetGlobalNotice :one
SELECT id, title, severity, body_md, body_html, urgent, urgent_sent_at, starts_at, ends_at, retired_at, created_by, created_at, updated_at
FROM global_notices
WHERE id = $1;

-- name: InsertGlobalNotice :one
INSERT INTO global_notices (title, severity, body_md, body_html, urgent, starts_at, ends_at, created_by, created_at, updated_at)
VALUES (sqlc.arg(title), sqlc.arg(severity), sqlc.arg(body_md), sqlc.arg(body_html), sqlc.arg(urgent), sqlc.arg(now), sqlc.arg(ends_at), sqlc.arg(created_by), sqlc.arg(now), sqlc.arg(now))
RETURNING id;

-- A retired notice is history and is not edited again.
-- name: UpdateGlobalNotice :execrows
UPDATE global_notices
SET title = sqlc.arg(title), severity = sqlc.arg(severity), body_md = sqlc.arg(body_md),
    body_html = sqlc.arg(body_html), ends_at = sqlc.arg(ends_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND retired_at IS NULL;

-- name: RetireGlobalNotice :execrows
UPDATE global_notices SET retired_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND retired_at IS NULL;

-- name: MarkGlobalNoticeUrgentSent :exec
UPDATE global_notices SET urgent_sent_at = sqlc.arg(now) WHERE id = sqlc.arg(id);

-- How many app-notification sends went out since a moment: the
-- urgent path's own, much tighter limit.
-- name: CountGlobalNoticeUrgentSince :one
SELECT COUNT(*)::bigint FROM global_notices WHERE urgent_sent_at >= sqlc.arg(since);

-- name: InsertGlobalNoticeAudit :exec
INSERT INTO global_notice_audit (notice_id, action, user_id, character_id, character_name, title, severity, body_md, ends_at, at)
VALUES (sqlc.arg(notice_id), sqlc.arg(action), sqlc.arg(user_id), sqlc.arg(character_id), sqlc.arg(character_name), sqlc.arg(title), sqlc.arg(severity), sqlc.arg(body_md), sqlc.arg(ends_at), sqlc.arg(at));

-- The rate limits: one account's actions of a kind since a moment,
-- and everyone's. A user_id of 0 counts every account.
-- name: CountGlobalNoticeAuditSince :one
SELECT COUNT(*)::bigint FROM global_notice_audit
WHERE action = sqlc.arg(action) AND at >= sqlc.arg(since)
  AND (sqlc.arg(user_id)::bigint = 0 OR user_id = sqlc.arg(user_id)::bigint);

-- name: ListGlobalNoticeAudit :many
SELECT id, notice_id, action, user_id, character_id, character_name, title, severity, ends_at, at
FROM global_notice_audit
ORDER BY at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: InsertGlobalNoticeDismissal :exec
INSERT INTO global_notice_dismissals (user_id, notice_id, dismissed_at)
SELECT sqlc.arg(user_id), id, sqlc.arg(now) FROM global_notices WHERE id = sqlc.arg(notice_id)
ON CONFLICT DO NOTHING;

-- name: ListGlobalNoticeDismissals :many
SELECT notice_id FROM global_notice_dismissals
WHERE user_id = sqlc.arg(user_id) AND notice_id = ANY(sqlc.arg(notice_ids)::bigint[]);

-- The urgent path's recipients: every account with a character.
-- name: ListGlobalNoticeRecipients :many
SELECT u.id FROM users u
WHERE EXISTS (SELECT 1 FROM characters c WHERE c.user_id = u.id)
ORDER BY u.id;
