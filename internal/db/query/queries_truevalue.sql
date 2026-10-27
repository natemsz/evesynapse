-- True values (schema 051): the worker's bottom-up price per
-- manufacturable type. build_cost is the per-unit cost of
-- building the type at the standard pricing profile (0 for
-- types with no manufacturing blueprint, priced from contract
-- evidence alone); est_sell is the estimated sell price for
-- types with no real market -- NULL until an evidence tier
-- stands behind a figure, because a build cost is a cost and
-- is never stored as a sell price. sell_samples counts the
-- samples behind est_sell (while it is NULL, every fresh
-- sample seen); sell_source names that tier (schema 052):
-- 'fulfilled' (linked characters' finished contracts --
-- certain sales), 'vanished' (watched listings gone before
-- expiry -- likely sold), 'listing' (live public asks),
-- '' while no tier has spoken.
-- Consumers resolve: The Forge's swept typical sell, then
-- est_sell, then unpriced (the guide average is never a price).
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
SELECT contract_id, region_id, type_id, shape, listed_price, sample_price, first_seen_at, last_seen_at, expires_at, vanished_at, sold
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

-- A watched listing ESI confirmed accepted (schema 070).
-- name: MarkTVContractObservationSold :exec
UPDATE true_value_contract_observations
SET vanished_at = $2, sold = TRUE
WHERE contract_id = $1;

-- name: DeleteTVContractObservation :exec
DELETE FROM true_value_contract_observations
WHERE contract_id = $1;

-- name: ListTVVanishedSamplesSince :many
SELECT contract_id, type_id, sample_price, vanished_at, sold
FROM true_value_contract_observations
WHERE vanished_at IS NOT NULL AND vanished_at >= $1
ORDER BY type_id, vanished_at;

-- Departed ship contracts awaiting their exit probe: watched
-- observations (the crawl's, and the ones the corpus miner
-- records) whose contract has left the stored public-contract
-- corpus -- gone from it, or kept there stamped departed
-- (schema 096) -- and whose expiry is still near enough for
-- ESI to answer whether it was accepted (a 204 before expiry
-- is a sale; see pub_contracts_sales.go). Freshest departures
-- first: ESI's memory of an accepted contract fades.
-- name: ListTVShipSaleProbes :many
SELECT o.contract_id, o.expires_at, o.type_id, o.sample_price, o.first_seen_at
FROM true_value_contract_observations o
WHERE o.vanished_at IS NULL
  AND o.type_id = ANY(sqlc.arg(ship_type_ids)::bigint[])
  AND o.expires_at > sqlc.arg(probe_after)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM pub_contracts c WHERE c.contract_id = o.contract_id
                  AND c.departed_at IS NULL)
ORDER BY o.last_seen_at DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: PurgeTVContractObservations :exec
DELETE FROM true_value_contract_observations
WHERE (vanished_at IS NULL AND expires_at < $1)
   OR (vanished_at IS NOT NULL AND vanished_at < $2);

-- name: ListTrueValuesForTypes :many
SELECT type_id, build_cost, est_sell, sell_samples, computed_at, profile_version, sell_source
FROM true_values
WHERE type_id = ANY($1::bigint[])
ORDER BY type_id;

-- The stored public-contract corpus's live item exchanges
-- carrying one type: the newest-seen contracts first, capped,
-- with every stored item line of each, so the true-value pass
-- can judge a contract's shape from the corpus exactly as it
-- judges a crawl fetch. Departed rows (schema 096) are left
-- out, so a contract's last_seen is its ask standing now.
--
-- The type's contracts are found from the item type index first and
-- only then sorted. Asked the other way round (newest contracts
-- first, each probed for the type), the planner walks the whole
-- corpus for a type on thousands of contracts: 24 ms a type against
-- 0.3 ms, and ~400 types a cycle.
-- name: ListTrueValueCorpusContracts :many
WITH typed AS MATERIALIZED (
    SELECT DISTINCT x.contract_id
    FROM pub_contract_items x
    WHERE x.type_id = $1 AND x.is_included
),
cand AS (
    SELECT c.contract_id, c.price, c.last_seen, c.region_id, c.date_expired, c.first_seen
    FROM typed
    JOIN pub_contracts c ON c.contract_id = typed.contract_id
    WHERE c.type = 'item_exchange' AND c.items_state = 'stored'
      AND c.departed_at IS NULL
      AND c.price >= $2
    ORDER BY c.last_seen DESC
    LIMIT $3
)
SELECT cand.contract_id, cand.price, cand.last_seen, cand.region_id, cand.date_expired, cand.first_seen,
       i.type_id, i.quantity, i.is_included
FROM cand
JOIN pub_contract_items i ON i.contract_id = cand.contract_id
ORDER BY cand.contract_id, i.type_id;
