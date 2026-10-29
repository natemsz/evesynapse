-- Market API (schema 108): keys, usage, abyssal wants, researched
-- blueprint medians.

-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, name, prefix, key_hash)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: CountLiveAPIKeys :one
SELECT COUNT(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL;

-- name: CountAPIKeysCreatedSince :one
SELECT COUNT(*) FROM api_keys WHERE user_id = $1 AND created_at >= sqlc.arg(since)::timestamptz;

-- name: ListAPIKeys :many
SELECT k.id, k.name, k.prefix, k.daily_quota, k.created_at, k.last_used_at,
       COALESCE(u.requests, 0)::bigint AS today_requests
FROM api_keys k
LEFT JOIN api_key_usage u ON u.key_id = k.id AND u.day = sqlc.arg(day)::text
WHERE k.user_id = sqlc.arg(user_id) AND k.revoked_at IS NULL
ORDER BY k.created_at DESC
LIMIT 20;

-- name: RevokeAPIKey :execrows
UPDATE api_keys SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: GetLiveAPIKeyByHash :one
SELECT k.id, k.user_id, k.daily_quota
FROM api_keys k
WHERE k.key_hash = $1 AND k.revoked_at IS NULL;

-- name: GetAPIKeyUsage :one
SELECT requests FROM api_key_usage WHERE key_id = $1 AND day = $2;

-- name: AddAPIKeyUsage :exec
INSERT INTO api_key_usage (key_id, day, requests, refused)
VALUES ($1, $2, $3, $4)
ON CONFLICT (key_id, day) DO UPDATE SET
    requests = api_key_usage.requests + excluded.requests,
    refused  = api_key_usage.refused + excluded.refused;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = $2 WHERE id = $1;

-- name: PruneAPIKeyUsage :exec
DELETE FROM api_key_usage WHERE day < sqlc.arg(before_day)::text;

-- name: AddAbyssalWant :execrows
-- A caller's request to price a mutated item the corpus has not
-- seen; a repeat request for the same item adds nothing.
INSERT INTO abyssal_wants (item_id, type_id, key_id, requested_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id) DO NOTHING;

-- name: CountAbyssalWantsSince :one
SELECT COUNT(*) FROM abyssal_wants WHERE key_id = $1 AND requested_at >= sqlc.arg(since)::timestamptz;

-- name: CountPendingAbyssalWants :one
SELECT COUNT(*) FROM abyssal_wants WHERE state = 'pending';

-- name: GetAbyssalWant :one
SELECT item_id, type_id, state, attempts FROM abyssal_wants WHERE item_id = $1;

-- name: ListAbyssalWantsByItems :many
SELECT item_id, type_id, state, attempts FROM abyssal_wants WHERE item_id = ANY(sqlc.arg(item_ids)::bigint[]);

-- name: ListPendingAbyssalWants :many
SELECT item_id, type_id, attempts FROM abyssal_wants
WHERE state = 'pending' AND (tried_at IS NULL OR tried_at < sqlc.arg(retry_before)::timestamptz)
ORDER BY requested_at
LIMIT sqlc.arg(row_cap)::bigint;

-- name: SetAbyssalWantState :exec
UPDATE abyssal_wants SET state = $2, attempts = attempts + 1, tried_at = $3 WHERE item_id = $1;

-- name: PruneAbyssalWants :exec
DELETE FROM abyssal_wants WHERE state <> 'pending' AND tried_at < sqlc.arg(before)::timestamptz;

-- name: ListBlueprintResearchMedianCandidates :many
-- Single-blueprint item exchanges whose one line is an included,
-- researched original (ME or TE above zero), grouped by type and
-- research level, at or over the minimum sample.
SELECT i.type_id AS type_id, i.material_efficiency AS me, i.time_efficiency AS te,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY c.price) AS median_price,
    COUNT(*) AS sample_count
FROM pub_contract_items i
JOIN pub_contracts c ON c.contract_id = i.contract_id
WHERE c.type = 'item_exchange'
  AND c.departed_at IS NULL
  AND c.price > 0
  AND i.is_included
  AND NOT i.is_blueprint_copy
  AND i.material_efficiency IS NOT NULL AND i.time_efficiency IS NOT NULL
  AND (i.material_efficiency > 0 OR i.time_efficiency > 0)
  AND i.quantity = 1
  AND (SELECT COUNT(*) FROM pub_contract_items x WHERE x.contract_id = i.contract_id) = 1
GROUP BY i.type_id, i.material_efficiency, i.time_efficiency
HAVING COUNT(*) >= sqlc.arg(min_sample)::bigint;

-- name: DeleteBlueprintResearchMedians :exec
DELETE FROM blueprint_research_medians;

-- name: InsertBlueprintResearchMedian :exec
INSERT INTO blueprint_research_medians (type_id, me, te, median_price, sample_count, computed_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListBlueprintResearchMediansForTypes :many
SELECT type_id, me, te, median_price, sample_count, computed_at
FROM blueprint_research_medians
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);
