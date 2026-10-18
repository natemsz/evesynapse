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
    days_to_complete, items_state, last_cycle, first_seen, last_seen,
    offered_value, want_value, deal_ratio, priced_lines, total_lines,
    tier_mix, priced_at
FROM pub_contracts
WHERE contract_id = $1;

-- ---------------------------------------------------------------------
-- Item lists: fetched once per contract, ever (a contract's
-- list is immutable once issued).
-- ---------------------------------------------------------------------

-- The item drain walks newest first: a freshly swept contract
-- gets its contents (and therefore its appraisal) within a
-- cycle or two, and the cold backlog backfills behind the new
-- listings in whatever budget is left. Parked rows (schema
-- 060: repeated fetch or decode failures) are out of the
-- selection for good.
-- name: ListPublicContractsPendingItems :many
SELECT contract_id, type FROM pub_contracts
WHERE items_state = 'pending'
ORDER BY date_issued DESC, contract_id DESC
LIMIT $1;

-- name: InsertPublicContractItem :exec
INSERT INTO pub_contract_items (
    record_id, contract_id, type_id, item_id, quantity, is_included,
    is_blueprint_copy, runs, material_efficiency, time_efficiency)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (record_id) DO NOTHING;

-- name: SetPublicContractItemsState :exec
UPDATE pub_contracts SET items_state = $2 WHERE contract_id = $1;

-- One failed item-list attempt (fetch, decode or store), on
-- the record: the counter rises, the reason is kept, and the
-- row parks itself once the counter reaches the drain's
-- threshold -- the caller passes both, so the policy lives in
-- one named constant in the warming pass.
-- name: NotePublicContractItemsFailure :exec
UPDATE pub_contracts
SET items_attempts = items_attempts + 1,
    items_error = LEFT(sqlc.arg(reason)::text, 500),
    items_state = CASE
        WHEN items_attempts + 1 >= sqlc.arg(max_attempts)::bigint THEN 'parked'
        ELSE items_state
    END
WHERE contract_id = sqlc.arg(contract_id)::bigint
  AND items_state = 'pending';

-- The drain's own ledger for a contract: how many attempts
-- its item list has failed and why the last one did. The test
-- suite reads it; the columns ride no page query.
-- name: GetPublicContractItemsLedger :one
SELECT items_state, items_attempts, items_error FROM pub_contracts
WHERE contract_id = $1;

-- name: ListPublicContractItems :many
SELECT record_id, contract_id, type_id, item_id, quantity, is_included,
    is_blueprint_copy, runs, material_efficiency, time_efficiency,
    unit_price, line_value, price_tier, price_asof
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
    attributes, fetched_at, first_seen_at, last_seen_at,
    family, score, percentiles, derived_stats,
    est_value, est_low, est_high, est_basis, est_scope, est_comps,
    premium_mult, cap_attribute_id, cap_kind,
    recreation_cost, recreation_attempts, primary_abs, scored_at
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
-- Appraisal (schema 056): the core appraisal pass prices stored
-- lines and stamps the contract's totals; the search and detail
-- pages read the stored results and never recompute.
-- ---------------------------------------------------------------------

-- The contracts whose appraisal is due: item list stored, and
-- either never priced or priced before the cutoff the caller
-- sets from the pricing inputs' cadence. Oldest appraisal
-- first, so a capped pass drains the stalest numbers first.
-- name: ListPublicContractsDueAppraisal :many
SELECT contract_id FROM pub_contracts
WHERE items_state = 'stored'
  AND (priced_at IS NULL OR priced_at < sqlc.arg(cutoff)::timestamptz)
ORDER BY priced_at ASC NULLS FIRST, contract_id ASC
LIMIT sqlc.arg(max_rows)::bigint;

-- name: UpdatePublicContractItemAppraisal :exec
UPDATE pub_contract_items
SET unit_price = sqlc.narg(unit_price)::double precision,
    line_value = sqlc.narg(line_value)::double precision,
    price_tier = sqlc.arg(price_tier)::text,
    price_asof = sqlc.narg(price_asof)::timestamptz
WHERE record_id = sqlc.arg(record_id)::bigint;

-- name: UpdatePublicContractAppraisal :exec
UPDATE pub_contracts
SET offered_value = sqlc.arg(offered_value)::double precision,
    want_value = sqlc.arg(want_value)::double precision,
    deal_ratio = sqlc.narg(deal_ratio)::double precision,
    priced_lines = sqlc.arg(priced_lines)::bigint,
    total_lines = sqlc.arg(total_lines)::bigint,
    tier_mix = sqlc.arg(tier_mix)::text,
    priced_at = sqlc.arg(priced_at)::timestamptz
WHERE contract_id = sqlc.arg(contract_id)::bigint;

-- The item rows of a page of search results in one read:
-- result rows carry no lines of their own, so the page reads
-- its contracts' lines in a single bounded follow-up.
-- name: ListPublicContractItemsForContracts :many
SELECT record_id, contract_id, type_id, item_id, quantity, is_included,
    is_blueprint_copy, runs, material_efficiency, time_efficiency,
    unit_price, line_value, price_tier, price_asof
FROM pub_contract_items
WHERE contract_id = ANY(sqlc.arg(contract_ids)::bigint[])
ORDER BY contract_id, record_id;

-- name: ListAbyssalItemsByIDs :many
SELECT item_id, type_id, source_type_id, mutator_type_id, created_by,
    attributes, fetched_at, first_seen_at, last_seen_at,
    family, score, percentiles, derived_stats,
    est_value, est_low, est_high, est_basis, est_scope, est_comps,
    premium_mult, cap_attribute_id, cap_kind,
    recreation_cost, recreation_attempts, primary_abs, scored_at
FROM abyssal_items
WHERE item_id = ANY(sqlc.arg(item_ids)::bigint[])
ORDER BY item_id;

-- The regions the search can offer: the ones holding live
-- contracts, with the SDE's name where it is known.
-- name: ListPublicContractRegions :many
SELECT DISTINCT c.region_id, COALESCE(r.name, '') AS region_name
FROM pub_contracts c
LEFT JOIN sde_regions r ON r.region_id = c.region_id
ORDER BY c.region_id;

-- ---------------------------------------------------------------------
-- Search (the contracts module's pages): every filter is a
-- clause over indexed columns; the sort is chosen by a key the
-- handler validates, never by raw input. Empty text means the
-- filter is off; array filters are off when empty; numeric
-- bands are off below zero.
-- ---------------------------------------------------------------------

-- name: SearchPublicContracts :many
SELECT c.contract_id, c.region_id, c.type, c.issuer_id,
    c.issuer_corporation_id, c.for_corporation, c.title, c.price,
    c.buyout, c.reward, c.collateral, c.volume,
    c.start_location_id, c.end_location_id, c.date_issued,
    c.date_expired, c.days_to_complete, c.items_state,
    c.last_cycle, c.first_seen, c.last_seen,
    c.offered_value, c.want_value, c.deal_ratio, c.priced_lines,
    c.total_lines, c.tier_mix, c.priced_at,
    (SELECT COALESCE(MAX(b.amount), 0)::double precision FROM pub_contract_bids b
      WHERE b.contract_id = c.contract_id) AS top_bid,
    (SELECT COUNT(*) FROM pub_contract_bids b
      WHERE b.contract_id = c.contract_id) AS bid_count
FROM pub_contracts c
WHERE (coalesce(cardinality(sqlc.arg(types)::text[]), 0) = 0
       OR c.type = ANY(sqlc.arg(types)::text[]))
  AND (coalesce(cardinality(sqlc.arg(region_ids)::bigint[]), 0) = 0
       OR c.region_id = ANY(sqlc.arg(region_ids)::bigint[]))
  AND (sqlc.arg(title_q)::text = ''
       OR c.title ILIKE '%' || sqlc.arg(title_q)::text || '%')
  AND (sqlc.arg(item_q)::text = '' OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        JOIN sde_types st ON st.type_id = i.type_id
        WHERE i.contract_id = c.contract_id
          AND st.name ILIKE '%' || sqlc.arg(item_q)::text || '%'
          AND (sqlc.arg(item_dir)::text = 'either'
               OR (sqlc.arg(item_dir)::text = 'offered' AND i.is_included)
               OR (sqlc.arg(item_dir)::text = 'wanted' AND NOT i.is_included))))
  AND (sqlc.arg(issuer_q)::text = ''
       OR c.issuer_id IN (SELECT id FROM entity_names
                          WHERE kind = 'character'
                            AND name ILIKE '%' || sqlc.arg(issuer_q)::text || '%')
       OR c.issuer_corporation_id IN (SELECT id FROM entity_names
                          WHERE kind = 'corporation'
                            AND name ILIKE '%' || sqlc.arg(issuer_q)::text || '%'))
  AND (sqlc.arg(for_corp)::bigint = -1
       OR c.for_corporation = (sqlc.arg(for_corp)::bigint = 1))
  AND (sqlc.arg(price_min)::double precision < 0
       OR (CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END)
            >= sqlc.arg(price_min)::double precision)
  AND (sqlc.arg(price_max)::double precision < 0
       OR (CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END)
            <= sqlc.arg(price_max)::double precision)
  AND (sqlc.arg(buyout_max)::double precision < 0
       OR c.buyout <= sqlc.arg(buyout_max)::double precision)
  AND (sqlc.arg(place_q)::text = ''
       OR c.start_location_id IN (
            SELECT station_id FROM sde_stations
            WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%'
            UNION
            SELECT structure_id FROM structure_names
            WHERE state = 'resolved'
              AND name ILIKE '%' || sqlc.arg(place_q)::text || '%'
            UNION
            SELECT station_id FROM sde_stations
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%')
            UNION
            SELECT structure_id FROM structure_context
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%')))
  AND (sqlc.arg(place_end_q)::text = ''
       OR c.end_location_id IN (
            SELECT station_id FROM sde_stations
            WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%'
            UNION
            SELECT structure_id FROM structure_names
            WHERE state = 'resolved'
              AND name ILIKE '%' || sqlc.arg(place_end_q)::text || '%'
            UNION
            SELECT station_id FROM sde_stations
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%')
            UNION
            SELECT structure_id FROM structure_context
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%')))
  AND (sqlc.arg(reward_min)::double precision < 0
       OR c.reward >= sqlc.arg(reward_min)::double precision)
  AND (sqlc.arg(reward_max)::double precision < 0
       OR c.reward <= sqlc.arg(reward_max)::double precision)
  AND (sqlc.arg(collateral_max)::double precision < 0
       OR c.collateral <= sqlc.arg(collateral_max)::double precision)
  AND (sqlc.arg(volume_min)::double precision < 0
       OR c.volume >= sqlc.arg(volume_min)::double precision)
  AND (sqlc.arg(volume_max)::double precision < 0
       OR c.volume <= sqlc.arg(volume_max)::double precision)
  AND (sqlc.arg(days_max)::bigint < 0
       OR c.days_to_complete <= sqlc.arg(days_max)::bigint)
  AND (NOT sqlc.arg(courier_active)::boolean OR c.type = 'courier')
  AND (NOT sqlc.arg(bp_active)::boolean OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        WHERE i.contract_id = c.contract_id AND i.is_included
          AND i.material_efficiency IS NOT NULL
          AND (sqlc.arg(bp_mode)::text = 'any'
               OR (sqlc.arg(bp_mode)::text = 'bpo' AND NOT i.is_blueprint_copy)
               OR (sqlc.arg(bp_mode)::text = 'bpc' AND i.is_blueprint_copy))
          AND (sqlc.arg(me_min)::bigint < 0
               OR i.material_efficiency >= sqlc.arg(me_min)::bigint)
          AND (sqlc.arg(te_min)::bigint < 0
               OR i.time_efficiency >= sqlc.arg(te_min)::bigint)
          AND (sqlc.arg(runs_min)::bigint < 0
               OR COALESCE(i.runs, 0) >= sqlc.arg(runs_min)::bigint)))
  AND (sqlc.arg(issued_after)::timestamptz IS NULL
       OR c.date_issued >= sqlc.arg(issued_after)::timestamptz)
  AND (sqlc.arg(expiring_before)::timestamptz IS NULL
       OR c.date_expired <= sqlc.arg(expiring_before)::timestamptz)
  AND (NOT sqlc.arg(abyssal_active)::boolean OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        JOIN abyssal_items a ON a.item_id = i.item_id
        WHERE i.contract_id = c.contract_id AND i.is_included
          AND (sqlc.arg(ab_family)::text = ''
               OR a.family = sqlc.arg(ab_family)::text)
          AND (sqlc.arg(ab_score_min)::double precision < 0
               OR a.score >= sqlc.arg(ab_score_min)::double precision)
          AND (sqlc.arg(ab_source_q)::text = '' OR EXISTS (
                SELECT 1 FROM sde_types st
                WHERE st.type_id = a.source_type_id
                  AND st.name ILIKE '%' || sqlc.arg(ab_source_q)::text || '%'))
          AND (sqlc.arg(ab_est_min)::double precision < 0
               OR a.est_value >= sqlc.arg(ab_est_min)::double precision)
          AND (sqlc.arg(ab_est_max)::double precision < 0
               OR a.est_value <= sqlc.arg(ab_est_max)::double precision)
          AND (sqlc.arg(ab_attr_key)::text = ''
               OR COALESCE((a.percentiles ->> sqlc.arg(ab_attr_key)::text)::double precision, -1)
                    >= sqlc.arg(ab_attr_min)::double precision)
          AND (sqlc.arg(ab_pos_min)::double precision < 0
               OR EXISTS (
                     SELECT 1 FROM abyssal_family_weights w
                     WHERE w.family = a.family AND NOT w.penalty_only
                       AND w.weight = (
                             SELECT MAX(w2.weight) FROM abyssal_family_weights w2
                             WHERE w2.family = a.family AND NOT w2.penalty_only)
                       AND COALESCE((a.percentiles ->> CAST(w.attribute_id AS text))::double precision, -1)
                             >= sqlc.arg(ab_pos_min)::double precision))))
  AND (NOT sqlc.arg(deals_only)::boolean
       OR (c.deal_ratio IS NOT NULL AND c.deal_ratio <= 0.85))
ORDER BY
  CASE WHEN sqlc.arg(sort)::text = 'price_asc' THEN
       CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'price_desc' THEN
       CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'margin_desc' THEN
       CASE WHEN c.type = 'courier' THEN c.offered_value - c.reward
            ELSE c.offered_value - c.price END
       END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'value_desc' THEN c.offered_value END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'deal_asc' THEN c.deal_ratio END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'expiry_asc' THEN c.date_expired END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'reward_desc' THEN c.reward END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'reward_m3_desc' THEN
       CASE WHEN c.volume > 0 THEN c.reward / c.volume END END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'score_desc' THEN
       (SELECT MAX(a.score) FROM pub_contract_items i
        JOIN abyssal_items a ON a.item_id = i.item_id
        WHERE i.contract_id = c.contract_id AND i.is_included) END DESC NULLS LAST,
  c.date_issued DESC, c.contract_id DESC
LIMIT sqlc.arg(max_rows)::bigint OFFSET sqlc.arg(row_offset)::bigint;

-- name: CountPublicContractsSearch :one
SELECT COUNT(*) FROM pub_contracts c
WHERE (coalesce(cardinality(sqlc.arg(types)::text[]), 0) = 0
       OR c.type = ANY(sqlc.arg(types)::text[]))
  AND (coalesce(cardinality(sqlc.arg(region_ids)::bigint[]), 0) = 0
       OR c.region_id = ANY(sqlc.arg(region_ids)::bigint[]))
  AND (sqlc.arg(title_q)::text = ''
       OR c.title ILIKE '%' || sqlc.arg(title_q)::text || '%')
  AND (sqlc.arg(item_q)::text = '' OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        JOIN sde_types st ON st.type_id = i.type_id
        WHERE i.contract_id = c.contract_id
          AND st.name ILIKE '%' || sqlc.arg(item_q)::text || '%'
          AND (sqlc.arg(item_dir)::text = 'either'
               OR (sqlc.arg(item_dir)::text = 'offered' AND i.is_included)
               OR (sqlc.arg(item_dir)::text = 'wanted' AND NOT i.is_included))))
  AND (sqlc.arg(issuer_q)::text = ''
       OR c.issuer_id IN (SELECT id FROM entity_names
                          WHERE kind = 'character'
                            AND name ILIKE '%' || sqlc.arg(issuer_q)::text || '%')
       OR c.issuer_corporation_id IN (SELECT id FROM entity_names
                          WHERE kind = 'corporation'
                            AND name ILIKE '%' || sqlc.arg(issuer_q)::text || '%'))
  AND (sqlc.arg(for_corp)::bigint = -1
       OR c.for_corporation = (sqlc.arg(for_corp)::bigint = 1))
  AND (sqlc.arg(price_min)::double precision < 0
       OR (CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END)
            >= sqlc.arg(price_min)::double precision)
  AND (sqlc.arg(price_max)::double precision < 0
       OR (CASE WHEN c.type = 'courier' THEN c.reward ELSE c.price END)
            <= sqlc.arg(price_max)::double precision)
  AND (sqlc.arg(buyout_max)::double precision < 0
       OR c.buyout <= sqlc.arg(buyout_max)::double precision)
  AND (sqlc.arg(place_q)::text = ''
       OR c.start_location_id IN (
            SELECT station_id FROM sde_stations
            WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%'
            UNION
            SELECT structure_id FROM structure_names
            WHERE state = 'resolved'
              AND name ILIKE '%' || sqlc.arg(place_q)::text || '%'
            UNION
            SELECT station_id FROM sde_stations
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%')
            UNION
            SELECT structure_id FROM structure_context
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_q)::text || '%')))
  AND (sqlc.arg(place_end_q)::text = ''
       OR c.end_location_id IN (
            SELECT station_id FROM sde_stations
            WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%'
            UNION
            SELECT structure_id FROM structure_names
            WHERE state = 'resolved'
              AND name ILIKE '%' || sqlc.arg(place_end_q)::text || '%'
            UNION
            SELECT station_id FROM sde_stations
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%')
            UNION
            SELECT structure_id FROM structure_context
            WHERE system_id IN (SELECT system_id FROM sde_systems
                                WHERE name ILIKE '%' || sqlc.arg(place_end_q)::text || '%')))
  AND (sqlc.arg(reward_min)::double precision < 0
       OR c.reward >= sqlc.arg(reward_min)::double precision)
  AND (sqlc.arg(reward_max)::double precision < 0
       OR c.reward <= sqlc.arg(reward_max)::double precision)
  AND (sqlc.arg(collateral_max)::double precision < 0
       OR c.collateral <= sqlc.arg(collateral_max)::double precision)
  AND (sqlc.arg(volume_min)::double precision < 0
       OR c.volume >= sqlc.arg(volume_min)::double precision)
  AND (sqlc.arg(volume_max)::double precision < 0
       OR c.volume <= sqlc.arg(volume_max)::double precision)
  AND (sqlc.arg(days_max)::bigint < 0
       OR c.days_to_complete <= sqlc.arg(days_max)::bigint)
  AND (NOT sqlc.arg(courier_active)::boolean OR c.type = 'courier')
  AND (NOT sqlc.arg(bp_active)::boolean OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        WHERE i.contract_id = c.contract_id AND i.is_included
          AND i.material_efficiency IS NOT NULL
          AND (sqlc.arg(bp_mode)::text = 'any'
               OR (sqlc.arg(bp_mode)::text = 'bpo' AND NOT i.is_blueprint_copy)
               OR (sqlc.arg(bp_mode)::text = 'bpc' AND i.is_blueprint_copy))
          AND (sqlc.arg(me_min)::bigint < 0
               OR i.material_efficiency >= sqlc.arg(me_min)::bigint)
          AND (sqlc.arg(te_min)::bigint < 0
               OR i.time_efficiency >= sqlc.arg(te_min)::bigint)
          AND (sqlc.arg(runs_min)::bigint < 0
               OR COALESCE(i.runs, 0) >= sqlc.arg(runs_min)::bigint)))
  AND (sqlc.arg(issued_after)::timestamptz IS NULL
       OR c.date_issued >= sqlc.arg(issued_after)::timestamptz)
  AND (sqlc.arg(expiring_before)::timestamptz IS NULL
       OR c.date_expired <= sqlc.arg(expiring_before)::timestamptz)
  AND (NOT sqlc.arg(abyssal_active)::boolean OR EXISTS (
        SELECT 1 FROM pub_contract_items i
        JOIN abyssal_items a ON a.item_id = i.item_id
        WHERE i.contract_id = c.contract_id AND i.is_included
          AND (sqlc.arg(ab_family)::text = ''
               OR a.family = sqlc.arg(ab_family)::text)
          AND (sqlc.arg(ab_score_min)::double precision < 0
               OR a.score >= sqlc.arg(ab_score_min)::double precision)
          AND (sqlc.arg(ab_source_q)::text = '' OR EXISTS (
                SELECT 1 FROM sde_types st
                WHERE st.type_id = a.source_type_id
                  AND st.name ILIKE '%' || sqlc.arg(ab_source_q)::text || '%'))
          AND (sqlc.arg(ab_est_min)::double precision < 0
               OR a.est_value >= sqlc.arg(ab_est_min)::double precision)
          AND (sqlc.arg(ab_est_max)::double precision < 0
               OR a.est_value <= sqlc.arg(ab_est_max)::double precision)
          AND (sqlc.arg(ab_attr_key)::text = ''
               OR COALESCE((a.percentiles ->> sqlc.arg(ab_attr_key)::text)::double precision, -1)
                    >= sqlc.arg(ab_attr_min)::double precision)
          AND (sqlc.arg(ab_pos_min)::double precision < 0
               OR EXISTS (
                     SELECT 1 FROM abyssal_family_weights w
                     WHERE w.family = a.family AND NOT w.penalty_only
                       AND w.weight = (
                             SELECT MAX(w2.weight) FROM abyssal_family_weights w2
                             WHERE w2.family = a.family AND NOT w2.penalty_only)
                       AND COALESCE((a.percentiles ->> CAST(w.attribute_id AS text))::double precision, -1)
                             >= sqlc.arg(ab_pos_min)::double precision))))
  AND (NOT sqlc.arg(deals_only)::boolean
       OR (c.deal_ratio IS NOT NULL AND c.deal_ratio <= 0.85));

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

-- ---------------------------------------------------------------------
-- Abyssal scoring (schema 057): the worker pass's reads and its
-- one stored write per item, the calibration corpus, and the
-- search page's abyssal option lists. Queries stay ASCII-only
-- in comments (the sqlc parser's known desync).
-- ---------------------------------------------------------------------

-- name: ListAllAbyssalFamilyWeights :many
SELECT family, attribute_id, weight, penalty_only
FROM abyssal_family_weights
ORDER BY family, attribute_id;

-- name: ListAbyssalFamilyWeightsByFamilies :many
SELECT family, attribute_id, weight, penalty_only
FROM abyssal_family_weights
WHERE family = ANY(sqlc.arg(families)::text[])
ORDER BY family, attribute_id;

-- The items whose score or estimate is due: never scored, or
-- scored before the caller's cutoff. Stalest first.
-- name: ListAbyssalItemsDueScoring :many
SELECT item_id, type_id, source_type_id, mutator_type_id, attributes
FROM abyssal_items
WHERE scored_at IS NULL
   OR scored_at < sqlc.arg(cutoff)::timestamptz
ORDER BY scored_at ASC NULLS FIRST, item_id ASC
LIMIT sqlc.arg(max_rows)::bigint;

-- The calibration corpus for one family: every scored item of
-- the family with its most recent exchange listing's ask and
-- how that listing ended. Auctions carry no comparable ask in
-- the sighting row (their price is an opening bid), so the
-- corpus is exchange listings only.
-- name: ListAbyssalCorpusByGroupNames :many
SELECT DISTINCT ON (a.item_id)
    a.item_id, a.source_type_id, a.score, a.est_value, a.primary_abs,
    s.price, s.departed_at, s.depart_kind,
    s.first_seen_at, s.last_seen_at, s.date_expired,
    g.name AS group_name
FROM abyssal_items a
JOIN abyssal_sightings s ON s.item_id = a.item_id
JOIN sde_types t ON t.type_id = a.type_id
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.name = ANY(sqlc.arg(group_names)::text[])
  AND s.contract_type = 'item_exchange'
  AND s.price > 0
ORDER BY a.item_id, s.first_seen_at DESC;

-- The base modules one mutaplasmid has ever been seen applied
-- to: the recreation path's candidate bases.
-- name: ListAbyssalSourcesForMutator :many
SELECT DISTINCT source_type_id FROM abyssal_items
WHERE mutator_type_id = $1 AND source_type_id > 0
ORDER BY source_type_id;

-- name: UpdateAbyssalItemEstimate :exec
UPDATE abyssal_items SET
    family              = sqlc.arg(family)::text,
    score               = sqlc.narg(score)::double precision,
    percentiles         = sqlc.arg(percentiles)::jsonb,
    derived_stats       = sqlc.arg(derived_stats)::jsonb,
    est_value           = sqlc.narg(est_value)::double precision,
    est_low             = sqlc.narg(est_low)::double precision,
    est_high            = sqlc.narg(est_high)::double precision,
    est_basis           = sqlc.arg(est_basis)::text,
    est_scope           = sqlc.arg(est_scope)::text,
    est_comps           = sqlc.arg(est_comps)::bigint,
    premium_mult        = sqlc.arg(premium_mult)::double precision,
    cap_attribute_id    = sqlc.arg(cap_attribute_id)::bigint,
    cap_kind            = sqlc.arg(cap_kind)::text,
    recreation_cost     = sqlc.narg(recreation_cost)::double precision,
    recreation_attempts = sqlc.narg(recreation_attempts)::double precision,
    primary_abs         = sqlc.narg(primary_abs)::double precision,
    scored_at           = sqlc.arg(scored_at)::timestamptz
WHERE item_id = sqlc.arg(item_id)::bigint;

-- A fresh score or estimate makes the contracts holding the
-- item due for repricing on the next appraisal pass.
-- name: TouchPublicContractsForItemIDs :exec
UPDATE pub_contracts SET priced_at = NULL
WHERE contract_id IN (
    SELECT contract_id FROM pub_contract_items
    WHERE item_id = ANY(sqlc.arg(item_ids)::bigint[]));

-- name: ListAbyssalFamilies :many
SELECT DISTINCT family FROM abyssal_items
WHERE family <> ''
ORDER BY family;

-- name: ListAbyssalWeightAttributeIDs :many
SELECT DISTINCT attribute_id FROM abyssal_family_weights
ORDER BY attribute_id;

-- ---------------------------------------------------------------------
-- Blueprint copy medians (schema 061): the worker's pass
-- distills the live corpus into one stored median ask per
-- (blueprint type, runs); the appraisal core reads the stored
-- rows and nothing else, and a copy line prices from them or
-- not at all.
-- ---------------------------------------------------------------------

-- The candidate medians over the live corpus: item-exchange
-- contracts whose ONE stored item row is an included copy
-- with known runs, asked above zero. A multi-line contract's
-- price is not a price for its copy, and a wanted copy was
-- never offered at the contract's price, so both stay out.
-- The minimum sample is the caller's: groups thinner than it
-- store nothing.
-- name: ListBlueprintCopyMedianCandidates :many
SELECT i.type_id AS type_id, i.runs AS runs,
    percentile_cont(0.5) WITHIN GROUP (ORDER BY c.price) AS median_price,
    COUNT(*) AS sample_count
FROM pub_contract_items i
JOIN pub_contracts c ON c.contract_id = i.contract_id
WHERE c.type = 'item_exchange'
  AND c.price > 0
  AND i.is_included
  AND i.is_blueprint_copy
  AND i.runs IS NOT NULL AND i.runs > 0
  AND i.quantity = 1
  AND (SELECT COUNT(*) FROM pub_contract_items x
       WHERE x.contract_id = i.contract_id) = 1
GROUP BY i.type_id, i.runs
HAVING COUNT(*) >= sqlc.arg(min_sample)::bigint;

-- name: DeleteBlueprintCopyMedians :exec
DELETE FROM blueprint_copy_medians;

-- name: InsertBlueprintCopyMedian :exec
INSERT INTO blueprint_copy_medians (type_id, runs, median_price, sample_count, computed_at)
VALUES ($1, $2, $3, $4, $5);

-- The stored rows' as-of: the copy-median pass recomputes at
-- most once per its refresh age, judged by this stamp. No
-- rows yet answers no row, which the caller reads as never.
-- name: GetBlueprintCopyMediansStamp :one
SELECT computed_at FROM blueprint_copy_medians
ORDER BY computed_at DESC
LIMIT 1;

-- name: ListBlueprintCopyMediansForTypes :many
SELECT type_id, runs, median_price, sample_count, computed_at
FROM blueprint_copy_medians
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);
