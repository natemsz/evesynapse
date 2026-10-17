-- ---------------------------------------------------------------------
-- Public contracts (schema 055): the worker's mirror of ESI's
-- public contract routes, plus the abyssal item corpus. Core
-- owns the fetch and these tables; the contracts module reads
-- them through its own queries when its pages land.
-- ---------------------------------------------------------------------

-- name: ListSDERegionIDs :many
SELECT region_id FROM sde_regions ORDER BY region_id;

-- name: GetPublicContractSweepState :one
SELECT region_id, next_page, total_pages, cycle, updated_at
FROM pub_contract_sweep_state
WHERE region_id = $1;

-- name: UpsertPublicContractSweepState :exec
INSERT INTO pub_contract_sweep_state (region_id, next_page, total_pages, cycle, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (region_id) DO UPDATE SET
    next_page   = excluded.next_page,
    total_pages = excluded.total_pages,
    cycle       = excluded.cycle,
    updated_at  = excluded.updated_at;

-- UpsertPublicContract refreshes a live contract's facts and
-- stamps the sweep cycle that saw it. first_seen and
-- items_state are deliberately not in the update: the first
-- sighting and the item-list state survive every refresh.
-- name: UpsertPublicContract :exec
INSERT INTO pub_contracts (
    contract_id, region_id, type, issuer_id, issuer_corporation_id,
    for_corporation, title, price, buyout, reward, collateral, volume,
    start_location_id, end_location_id, date_issued, date_expired,
    days_to_complete, last_cycle, first_seen, last_seen)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $19)
ON CONFLICT (contract_id) DO UPDATE SET
    region_id             = excluded.region_id,
    type                  = excluded.type,
    issuer_id             = excluded.issuer_id,
    issuer_corporation_id = excluded.issuer_corporation_id,
    for_corporation       = excluded.for_corporation,
    title                 = excluded.title,
    price                 = excluded.price,
    buyout                = excluded.buyout,
    reward                = excluded.reward,
    collateral            = excluded.collateral,
    volume                = excluded.volume,
    start_location_id     = excluded.start_location_id,
    end_location_id       = excluded.end_location_id,
    date_issued           = excluded.date_issued,
    date_expired          = excluded.date_expired,
    days_to_complete      = excluded.days_to_complete,
    last_cycle            = excluded.last_cycle,
    last_seen             = excluded.last_seen;

-- The contracts of one region that leave the live set when a
-- sweep completes: expiry passed, or not seen in the completed
-- cycle nor the one before it (last_cycle at or below the
-- stale bound). depart_kind says which, for the sighting
-- history; nothing here guesses why a vanished contract left.
-- name: ListDepartedPublicContracts :many
SELECT contract_id,
    CASE WHEN date_expired <= $2 THEN 'expired' ELSE 'vanished' END AS depart_kind
FROM pub_contracts
WHERE region_id = $1
  AND (date_expired <= $2 OR last_cycle <= $3)
ORDER BY contract_id;

-- name: DeletePublicContractsByIDs :exec
DELETE FROM pub_contracts
WHERE contract_id = ANY(sqlc.arg(contract_ids)::bigint[]);

-- name: DeletePublicContractItemsByContracts :exec
DELETE FROM pub_contract_items
WHERE contract_id = ANY(sqlc.arg(contract_ids)::bigint[]);

-- name: DeletePublicContractBidsByContracts :exec
DELETE FROM pub_contract_bids
WHERE contract_id = ANY(sqlc.arg(contract_ids)::bigint[]);

-- name: CountPublicContracts :one
SELECT COUNT(*) FROM pub_contracts;

-- name: GetPublicContract :one
SELECT contract_id, region_id, type, issuer_id, issuer_corporation_id,
    for_corporation, title, price, buyout, reward, collateral, volume,
    start_location_id, end_location_id, date_issued, date_expired,
    days_to_complete, items_state, last_cycle, first_seen, last_seen
FROM pub_contracts
WHERE contract_id = $1;

-- ---------------------------------------------------------------------
-- Item lists: fetched once per contract, ever (a contract's
-- list is immutable once issued).
-- ---------------------------------------------------------------------

-- name: ListPublicContractsPendingItems :many
SELECT contract_id, type FROM pub_contracts
WHERE items_state = 'pending'
ORDER BY first_seen ASC, contract_id ASC
LIMIT $1;

-- name: InsertPublicContractItem :exec
INSERT INTO pub_contract_items (
    record_id, contract_id, type_id, item_id, quantity, is_included,
    is_blueprint_copy, runs, material_efficiency, time_efficiency)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (record_id) DO NOTHING;

-- name: SetPublicContractItemsState :exec
UPDATE pub_contracts SET items_state = $2 WHERE contract_id = $1;

-- name: ListPublicContractItems :many
SELECT record_id, contract_id, type_id, item_id, quantity, is_included,
    is_blueprint_copy, runs, material_efficiency, time_efficiency
FROM pub_contract_items
WHERE contract_id = $1
ORDER BY record_id;

-- name: CountPublicContractItems :one
SELECT COUNT(*) FROM pub_contract_items;

-- ---------------------------------------------------------------------
-- Bids: live auctions only, refreshed on the list route's
-- cadence; bidders are anonymous in ESI and stay that way.
-- ---------------------------------------------------------------------

-- name: ListPublicContractLiveAuctions :many
SELECT contract_id FROM pub_contracts
WHERE type = 'auction' AND date_expired > $1
ORDER BY date_expired ASC, contract_id ASC
LIMIT $2;

-- name: DeletePublicContractBids :exec
DELETE FROM pub_contract_bids WHERE contract_id = $1;

-- name: InsertPublicContractBid :exec
INSERT INTO pub_contract_bids (bid_id, contract_id, amount, date_bid)
VALUES ($1, $2, $3, $4)
ON CONFLICT (bid_id) DO NOTHING;

-- name: ListPublicContractBids :many
SELECT bid_id, contract_id, amount, date_bid FROM pub_contract_bids
WHERE contract_id = $1
ORDER BY amount DESC, date_bid ASC;

-- ---------------------------------------------------------------------
-- Abyssal corpus: probes (every outcome stored), items
-- (fetched once ever), sightings (one row per listing of an
-- instance, with its departure recorded when it leaves).
-- ---------------------------------------------------------------------

-- Singleton items on live contracts that no probe has ever
-- touched, oldest listing first. The contract facts ride
-- along so a hit can open its sighting row without a reread.
-- name: ListAbyssalProbeCandidates :many
SELECT i.item_id, i.type_id, i.contract_id,
    c.type, c.price, c.region_id, c.start_location_id,
    c.date_expired, c.first_seen
FROM pub_contract_items i
JOIN pub_contracts c ON c.contract_id = i.contract_id
LEFT JOIN abyssal_probes p ON p.item_id = i.item_id
WHERE i.item_id IS NOT NULL AND p.item_id IS NULL
ORDER BY c.first_seen ASC, i.item_id ASC
LIMIT $1;

-- name: InsertAbyssalProbe :exec
INSERT INTO abyssal_probes (item_id, type_id, is_dynamic, probed_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id) DO NOTHING;

-- An instance's rolls never change: the first fetch is the
-- row; later sightings of the same instance only move
-- last_seen_at.
-- name: UpsertAbyssalItem :exec
INSERT INTO abyssal_items (
    item_id, type_id, source_type_id, mutator_type_id, created_by,
    attributes, fetched_at, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
ON CONFLICT (item_id) DO UPDATE SET
    last_seen_at = excluded.last_seen_at;

-- name: GetAbyssalItem :one
SELECT item_id, type_id, source_type_id, mutator_type_id, created_by,
    attributes, fetched_at, first_seen_at, last_seen_at
FROM abyssal_items
WHERE item_id = $1;

-- name: CountAbyssalItems :one
SELECT COUNT(*) FROM abyssal_items;

-- name: UpsertAbyssalSighting :exec
INSERT INTO abyssal_sightings (
    item_id, contract_id, contract_type, price, region_id,
    location_id, date_expired, first_seen_at, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (item_id, contract_id) DO UPDATE SET
    price        = excluded.price,
    region_id    = excluded.region_id,
    location_id  = excluded.location_id,
    date_expired = excluded.date_expired,
    last_seen_at = excluded.last_seen_at;

-- When a contract's item list lands, any already-known
-- abyssal instance on it opens its sighting for the listing
-- right away (a relist of a probed instance never re-probes,
-- and the sweep-completion refresh below only sees listings
-- whose items were stored before it ran).
-- name: OpenAbyssalSightingsForContract :exec
INSERT INTO abyssal_sightings (
    item_id, contract_id, contract_type, price, region_id,
    location_id, date_expired, first_seen_at, last_seen_at)
SELECT i.item_id, c.contract_id, c.type, c.price, c.region_id,
    c.start_location_id, c.date_expired, c.first_seen, $2
FROM pub_contracts c
JOIN pub_contract_items i ON i.contract_id = c.contract_id
JOIN abyssal_items a ON a.item_id = i.item_id
WHERE c.contract_id = $1 AND i.item_id IS NOT NULL
ON CONFLICT (item_id, contract_id) DO UPDATE SET
    price        = excluded.price,
    region_id    = excluded.region_id,
    location_id  = excluded.location_id,
    date_expired = excluded.date_expired,
    last_seen_at = excluded.last_seen_at;

-- Every known abyssal item on a contract this region's
-- completed sweep just saw gets (or refreshes) its sighting
-- row for that listing, straight from the stored tables.
-- name: RefreshAbyssalSightingsForRegionCycle :exec
INSERT INTO abyssal_sightings (
    item_id, contract_id, contract_type, price, region_id,
    location_id, date_expired, first_seen_at, last_seen_at)
SELECT i.item_id, c.contract_id, c.type, c.price, c.region_id,
    c.start_location_id, c.date_expired, c.first_seen, $3
FROM pub_contracts c
JOIN pub_contract_items i ON i.contract_id = c.contract_id
JOIN abyssal_items a ON a.item_id = i.item_id
WHERE c.region_id = $1 AND c.last_cycle = $2 AND i.item_id IS NOT NULL
ON CONFLICT (item_id, contract_id) DO UPDATE SET
    price        = excluded.price,
    region_id    = excluded.region_id,
    location_id  = excluded.location_id,
    date_expired = excluded.date_expired,
    last_seen_at = excluded.last_seen_at;

-- The departure stamp also snapshots the listing's bid
-- state (an auction's rows in pub_contract_bids, counted
-- before the completion step deletes them): an exit with
-- bids on it is a different fact from an exit without.
-- name: StampAbyssalSightingsDeparted :exec
UPDATE abyssal_sightings s
SET departed_at = sqlc.arg(departed_at)::timestamptz,
    depart_kind = sqlc.arg(depart_kind)::text,
    bid_count = (SELECT COUNT(*) FROM pub_contract_bids b
                 WHERE b.contract_id = s.contract_id),
    top_bid = (SELECT MAX(b.amount) FROM pub_contract_bids b
               WHERE b.contract_id = s.contract_id)
WHERE s.contract_id = ANY(sqlc.arg(contract_ids)::bigint[])
  AND s.departed_at IS NULL;

-- name: ListAbyssalSightingsForItem :many
SELECT item_id, contract_id, contract_type, price, region_id,
    location_id, date_expired,
    first_seen_at, last_seen_at, departed_at, depart_kind,
    bid_count, top_bid
FROM abyssal_sightings
WHERE item_id = $1
ORDER BY first_seen_at, contract_id;

-- ---------------------------------------------------------------------
-- Roll ranges (schema 055), kept filled by the SDE importer.
-- ---------------------------------------------------------------------

-- name: ListAbyssalRangesForMutator :many
SELECT mutator_type_id, attribute_id, min_mult, max_mult, sde_build
FROM abyssal_ranges
WHERE mutator_type_id = $1
ORDER BY attribute_id;

-- name: CountAbyssalRanges :one
SELECT COUNT(*) FROM abyssal_ranges;
