-- True values (schema 051): the worker's bottom-up price per
-- manufacturable type. build_cost is the per-unit cost of
-- building the type at the standard pricing profile; est_sell
-- is the estimated sell price for types with no real market
-- (NULL while the market price rules); sell_samples counts the
-- public contracts behind est_sell (0 = anchored to build
-- cost). Consumers resolve: average, then est_sell, then
-- adjusted. Written only by the worker's true-value pass.

-- name: UpsertTrueValue :exec
INSERT INTO true_values (type_id, build_cost, est_sell, sell_samples, computed_at, profile_version)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (type_id) DO UPDATE SET
    build_cost = EXCLUDED.build_cost,
    est_sell = EXCLUDED.est_sell,
    sell_samples = EXCLUDED.sell_samples,
    computed_at = EXCLUDED.computed_at,
    profile_version = EXCLUDED.profile_version;

-- name: GetTrueValue :one
SELECT type_id, build_cost, est_sell, sell_samples, computed_at, profile_version
FROM true_values
WHERE type_id = $1;

-- name: ListTrueValues :many
SELECT type_id, build_cost, est_sell, sell_samples, computed_at, profile_version
FROM true_values
ORDER BY type_id;
