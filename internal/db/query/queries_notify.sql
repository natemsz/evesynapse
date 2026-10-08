-- ---------------------------------------------------------------------
-- Notifications (schema 012). The worker writes; pages read and mark
-- read. notification_seen is the worker's memory of the events it
-- has already considered.
-- ---------------------------------------------------------------------

-- name: ListNotificationSeenKeys :many
SELECT event_key FROM notification_seen
WHERE user_id = sqlc.arg(user_id) AND event_key = ANY(sqlc.arg(event_keys)::text[]);

-- name: InsertNotificationSeen :exec
INSERT INTO notification_seen (user_id, event_key, seen_at)
SELECT sqlc.arg(user_id), unnest(sqlc.arg(event_keys)::text[]), sqlc.arg(seen_at)
ON CONFLICT DO NOTHING;

-- Keys still current are kept alive; touched at most once a day each.
-- name: TouchNotificationSeen :exec
UPDATE notification_seen SET seen_at = sqlc.arg(seen_at)
WHERE user_id = sqlc.arg(user_id)
  AND event_key = ANY(sqlc.arg(event_keys)::text[])
  AND seen_at < sqlc.arg(stale_before);

-- A state that has cleared is forgotten, so it is announced again
-- the next time it holds.
-- name: ForgetNotificationSeenExcept :exec
DELETE FROM notification_seen
WHERE user_id = sqlc.arg(user_id)
  AND event_key LIKE sqlc.arg(key_pattern)
  AND NOT (event_key = ANY(sqlc.arg(keep_keys)::text[]));

-- name: PruneNotificationSeen :exec
DELETE FROM notification_seen WHERE seen_at < sqlc.arg(seen_before);

-- name: InsertNotification :one
INSERT INTO notifications (user_id, character_id, kind, title, url, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id;

-- One row per kind with something unread: how many, and the newest
-- one's text and address. This is the top-bar icon's whole read.
-- name: ListUnreadNotificationSummary :many
SELECT DISTINCT ON (kind) kind, title, url,
       (COUNT(*) OVER (PARTITION BY kind))::bigint AS unread
FROM notifications
WHERE user_id = $1 AND read_at IS NULL
ORDER BY kind, id DESC;

-- name: ListNotifications :many
SELECT id, user_id, character_id, kind, title, url, created_at, read_at FROM notifications
WHERE user_id = $1
ORDER BY id DESC
LIMIT $2;

-- name: MarkNotificationsRead :exec
UPDATE notifications SET read_at = sqlc.arg(read_at)
WHERE user_id = sqlc.arg(user_id) AND read_at IS NULL;

-- name: MarkNotificationsReadByKind :exec
UPDATE notifications SET read_at = sqlc.arg(read_at)
WHERE user_id = sqlc.arg(user_id) AND kind = sqlc.arg(kind) AND read_at IS NULL;

-- Read ones go after a while; unread ones are kept longer but not
-- for ever.
-- name: PruneNotifications :exec
DELETE FROM notifications
WHERE (read_at IS NOT NULL AND read_at < sqlc.arg(read_before))
   OR created_at < sqlc.arg(created_before);
