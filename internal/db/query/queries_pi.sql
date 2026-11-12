-- name: InsertPIPlan :exec
INSERT INTO pi_plans (token, user_id, request, solution)
VALUES ($1, $2, $3, $4);

-- name: GetPIPlan :one
SELECT token, user_id, created_at, request, solution FROM pi_plans
WHERE token = $1;

-- name: ListPIPlansByUser :many
-- An account's recent plans, newest first, for the planner's list.
SELECT token, created_at, request FROM pi_plans
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT 10;

-- name: DeleteStalePIPlans :exec
-- Clears plans made without an account after 30 days, a bounded slice
-- per call.
DELETE FROM pi_plans WHERE token IN (
    SELECT token FROM pi_plans
    WHERE user_id = 0 AND created_at < now() - interval '30 days'
    LIMIT 200
);
