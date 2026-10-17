-- True values (schema 051): the worker's bottom-up price per
-- manufacturable type. build_cost is the per-unit cost of
-- building the type at the standard pricing profile; est_sell
-- is the estimated sell price for types with no real market
-- (NULL while the market price rules); sell_samples counts the
-- samples of the tier behind est_sell (0 = anchored to build
-- cost); sell_source names that tier (schema 052): 'fulfilled'
-- (linked characters' finished contracts -- certain sales),
-- 'vanished' (watched listings gone before expiry -- likely
-- sold), 'listing' (live public asks), '' while anchored.
-- Consumers resolve: average, then est_sell, then adjusted.
-- Written only by the worker's true-value pass.

-- name: UpsertTrueValue :exec
INSERT INTO true_values (type_id, build_cost, est_sell, sell_samples, computed_at, profile_version, sell_source)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (type_id) DO UPDATE SET
    build_cost = EXCLUDED.build_cost,
    est_sell = EXCLUDED.est_sell,
    sell_samples = EXCLUDED.sell_samples,
    computed_at = EXCLUDED.computed_at,
    profile_version = EXCLUDED.profile_version,
    sell_source = EXCLUDED.sell_source;

-- name: GetTrueValue :one
SELECT type_id, build_cost, est_sell, sell_samples, computed_at, profile_version, sell_source
FROM true_values
WHERE type_id = $1;

-- name: ListTrueValues :many
SELECT type_id, build_cost, est_sell, sell_samples, computed_at, profile_version, sell_source
FROM true_values
ORDER BY type_id;

-- Contract observations (schema 052): the crawl's memory of
-- every public contract the sampler accepted, kept so later
-- crawls can tell whether the listing is still there. A
-- listing that disappears well before its expiry is marked
-- vanished (probable-sale evidence); rows are bookkeeping
-- only and are never read by a page.

-- name: UpsertTVContractObservation :exec
INSERT INTO true_value_contract_observations
    (contract_id, region_id, type_id, shape, listed_price, sample_price, first_seen_at, last_seen_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7, $8)
ON CONFLICT (contract_id) DO UPDATE SET
    region_id = EXCLUDED.region_id,
    type_id = EXCLUDED.type_id,
    shape = EXCLUDED.shape,
    listed_price = EXCLUDED.listed_price,
    sample_price = EXCLUDED.sample_price,
    last_seen_at = EXCLUDED.last_seen_at,
    expires_at = EXCLUDED.expires_at;

-- name: ListTVContractObservationsByRegion :many
SELECT contract_id, region_id, type_id, shape, listed_price, sample_price, first_seen_at, last_seen_at, expires_at, vanished_at
FROM true_value_contract_observations
WHERE region_id = $1 AND vanished_at IS NULL
ORDER BY last_seen_at;

-- name: TouchTVContractObservation :exec
UPDATE true_value_contract_observations
SET last_seen_at = $2
WHERE contract_id = $1;

-- name: MarkTVContractObservationVanished :exec
UPDATE true_value_contract_observations
SET vanished_at = $2
WHERE contract_id = $1;

-- name: DeleteTVContractObservation :exec
DELETE FROM true_value_contract_observations
WHERE contract_id = $1;

-- name: ListTVVanishedSamplesSince :many
SELECT type_id, sample_price, vanished_at
FROM true_value_contract_observations
WHERE vanished_at IS NOT NULL AND vanished_at >= $1
ORDER BY type_id, vanished_at;

-- name: PurgeTVContractObservations :exec
DELETE FROM true_value_contract_observations
WHERE (vanished_at IS NULL AND expires_at < $1)
   OR (vanished_at IS NOT NULL AND vanished_at < $2);
