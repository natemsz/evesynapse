-- ---------------------------------------------------------------------
-- Foresight (schema 027): data warmed because of a game event.
-- ---------------------------------------------------------------------

-- An event is acted on once: the same target for the same event adds
-- nothing.
-- name: AddForesightTarget :execrows
INSERT INTO foresight_targets (user_id, character_id, event_kind, event_key, target_kind, subject_id, region_id, detail, view_key, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT DO NOTHING;

-- name: ListForesightPending :many
SELECT * FROM foresight_targets
WHERE warmed_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::bigint;

-- name: MarkForesightWarmed :exec
UPDATE foresight_targets SET warmed_at = sqlc.arg(warmed_at), calls = sqlc.arg(calls) WHERE id = sqlc.arg(id);

-- The account opened the page a target was warmed for.
-- name: MarkForesightOpened :execrows
UPDATE foresight_targets SET opened_at = sqlc.arg(opened_at)
WHERE user_id = sqlc.arg(user_id) AND view_key = sqlc.arg(view_key)
  AND opened_at IS NULL AND expires_at > sqlc.arg(opened_at);

-- name: CountForesightLive :one
SELECT COUNT(*)::bigint FROM foresight_targets WHERE opened_at IS NULL AND expires_at > sqlc.arg(now);

-- How targets that have run their course turned out, by kind of event:
-- what the chances are learned from.
-- name: ListForesightRates :many
SELECT event_kind, COUNT(*)::bigint AS resolved, COUNT(opened_at)::bigint AS opened
FROM foresight_targets
WHERE created_at >= sqlc.arg(since) AND (opened_at IS NOT NULL OR expires_at <= sqlc.arg(now))
GROUP BY event_kind;

-- The same for some accounts, each on its own.
-- name: ListForesightUserRates :many
SELECT user_id, event_kind, COUNT(*)::bigint AS resolved, COUNT(opened_at)::bigint AS opened
FROM foresight_targets
WHERE user_id = ANY(sqlc.arg(user_ids)::bigint[])
  AND created_at >= sqlc.arg(since) AND (opened_at IS NOT NULL OR expires_at <= sqlc.arg(now))
GROUP BY user_id, event_kind;

-- The Sync page's table.
-- name: ListForesightSummary :many
SELECT event_kind,
       COUNT(DISTINCT (user_id, event_key))::bigint AS events,
       COUNT(*)::bigint AS targets,
       COALESCE(SUM(calls), 0)::bigint AS calls,
       COUNT(opened_at)::bigint AS opened,
       COUNT(*) FILTER (WHERE opened_at IS NOT NULL AND warmed_at IS NOT NULL AND warmed_at <= opened_at)::bigint AS opened_warm,
       COUNT(*) FILTER (WHERE opened_at IS NOT NULL OR expires_at <= sqlc.arg(now))::bigint AS resolved
FROM foresight_targets
WHERE created_at >= sqlc.arg(since)
GROUP BY event_kind
ORDER BY event_kind;

-- name: PruneForesightTargets :exec
DELETE FROM foresight_targets WHERE created_at < sqlc.arg(before);

-- Wars declared since a date, as stored.
-- name: ListWarsDeclaredSince :many
SELECT war_id, payload FROM war_details
WHERE (payload::jsonb ->> 'declared')::timestamptz >= sqlc.arg(since)::timestamptz
ORDER BY war_id DESC
LIMIT sqlc.arg(row_limit)::bigint;

-- The accounts with a character in one of some corporations.
-- name: ListUsersInCorporations :many
SELECT DISTINCT c.user_id, cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE cc.corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[]);
