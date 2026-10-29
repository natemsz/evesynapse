-- LP store browser (schema 102): the mirrored NPC loyalty stores
-- and each offer's stored value per swept hub region.

-- name: UpsertLPStoreCorp :exec
-- Seeds a corporation from ESI's NPC corporation list; an existing
-- row keeps its state and stamp.
INSERT INTO lp_store_corps (corporation_id)
VALUES ($1)
ON CONFLICT (corporation_id) DO NOTHING;

-- name: ListDueLPStoreCorps :many
-- The stores whose offers are due a re-read, never-read first.
SELECT corporation_id FROM lp_store_corps
WHERE fetched_at IS NULL OR fetched_at < sqlc.arg(cutoff)
ORDER BY fetched_at NULLS FIRST, corporation_id
LIMIT sqlc.arg(row_cap)::bigint;

-- name: MarkLPStoreCorp :exec
UPDATE lp_store_corps SET state = $2, offer_count = $3, fetched_at = $4
WHERE corporation_id = $1;

-- name: ListUnnamedLPStoreCorps :many
SELECT corporation_id FROM lp_store_corps
WHERE name = '' AND offer_count > 0
ORDER BY corporation_id
LIMIT 500;

-- name: SetLPStoreCorpName :exec
UPDATE lp_store_corps SET name = $2 WHERE corporation_id = $1;

-- name: ListLPOffersByCorp :many
SELECT offer_id, type_id, quantity, lp_cost, isk_cost, ak_cost FROM lp_offers
WHERE corporation_id = $1
ORDER BY offer_id;

-- name: ListLPOfferRequirementsByCorp :many
SELECT offer_id, type_id, quantity FROM lp_offer_requirements
WHERE corporation_id = $1
ORDER BY offer_id, type_id;

-- name: DeleteLPOffersByCorp :exec
DELETE FROM lp_offers WHERE corporation_id = $1;

-- name: DeleteLPOfferRequirementsByCorp :exec
DELETE FROM lp_offer_requirements WHERE corporation_id = $1;

-- name: InsertLPOffer :exec
INSERT INTO lp_offers (corporation_id, offer_id, type_id, quantity, lp_cost, isk_cost, ak_cost)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (corporation_id, offer_id) DO NOTHING;

-- name: InsertLPOfferRequirement :exec
-- A requirement listed twice for one offer adds up.
INSERT INTO lp_offer_requirements (corporation_id, offer_id, type_id, quantity)
VALUES ($1, $2, $3, $4)
ON CONFLICT (corporation_id, offer_id, type_id) DO UPDATE SET
    quantity = lp_offer_requirements.quantity + excluded.quantity;

-- name: ListLPStoreCorps :many
-- The page's store picker: stores that sell something.
SELECT corporation_id, name, offer_count FROM lp_store_corps
WHERE offer_count > 0
ORDER BY CASE WHEN name = '' THEN 1 ELSE 0 END, name, corporation_id;

-- name: DeleteLPOfferValuesByRegion :exec
DELETE FROM lp_offer_values WHERE region_id = $1;

-- name: RefreshLPOfferValues :execrows
-- Values every mirrored offer in one region from the stat pack's
-- stored rows. The product sells at its typical sell (into sell
-- orders) or its typical buy (at once into buy orders); every
-- required item is bought at its typical sell. Unpriced when the
-- product is a blueprint copy (its original's market is not its
-- price), when the product has no typical price, or when any
-- required item has none.
INSERT INTO lp_offer_values (region_id, corporation_id, offer_id, type_id, quantity, lp_cost, isk_cost,
    required_cost, unit_sell, unit_buy, isk_per_lp_sell, isk_per_lp_buy, unpriced_reason,
    daily_volume, sell_chg_7d, volatility_30d, prices_at, updated_at)
SELECT sqlc.arg(region_id)::bigint, v.corporation_id, v.offer_id, v.type_id, v.quantity, v.lp_cost, v.isk_cost,
       v.required_cost, v.unit_sell, v.unit_buy,
       CASE WHEN v.reason = '' AND v.lp_cost > 0 AND v.unit_sell > 0
            THEN (v.quantity * v.unit_sell - v.isk_cost - v.required_cost) / v.lp_cost END,
       CASE WHEN v.reason = '' AND v.lp_cost > 0 AND v.unit_buy > 0
            THEN (v.quantity * v.unit_buy - v.isk_cost - v.required_cost) / v.lp_cost END,
       v.reason, v.daily_volume, v.sell_chg_7d, v.volatility_30d, v.prices_at, sqlc.arg(now)::timestamptz
FROM (
    SELECT o.corporation_id, o.offer_id, o.type_id, o.quantity, o.lp_cost, o.isk_cost,
           COALESCE(r.cost, 0) AS required_cost,
           COALESCE(s.typical_sell, 0) AS unit_sell,
           COALESCE(s.typical_buy, 0) AS unit_buy,
           CASE
               WHEN COALESCE(g.category_id, 0) = 9 THEN 'blueprint copy'
               WHEN COALESCE(s.typical_sell, 0) <= 0 AND COALESCE(s.typical_buy, 0) <= 0 THEN 'no market for the item'
               WHEN COALESCE(r.missing, 0) > 0 THEN 'a required item has no market'
               ELSE ''
           END AS reason,
           COALESCE(s.avg_daily_volume, 0) AS daily_volume,
           s.sell_chg_7d, s.volatility_30d, s.updated_at AS prices_at
    FROM lp_offers o
    LEFT JOIN market_region_stats s ON s.region_id = sqlc.arg(region_id)::bigint AND s.type_id = o.type_id
    LEFT JOIN sde_types t ON t.type_id = o.type_id
    LEFT JOIN sde_groups g ON g.group_id = t.group_id
    LEFT JOIN (
        SELECT q.corporation_id, q.offer_id,
               SUM(q.quantity * COALESCE(rs.typical_sell, 0)) AS cost,
               COUNT(*) FILTER (WHERE COALESCE(rs.typical_sell, 0) <= 0) AS missing
        FROM lp_offer_requirements q
        LEFT JOIN market_region_stats rs ON rs.region_id = sqlc.arg(region_id)::bigint AND rs.type_id = q.type_id
        GROUP BY q.corporation_id, q.offer_id
    ) r ON r.corporation_id = o.corporation_id AND r.offer_id = o.offer_id
) v;

-- name: ListLPOfferValues :many
-- The browser's rows: one region, optionally one store, priced
-- offers over the daily-volume floor, best ISK per LP first.
-- Ranked on the sell side or the instant-sale side.
SELECT v.corporation_id, c.name AS corporation_name, v.offer_id, v.type_id, v.quantity, v.lp_cost, v.isk_cost,
       v.required_cost, v.unit_sell, v.unit_buy, v.isk_per_lp_sell, v.isk_per_lp_buy,
       v.daily_volume, v.sell_chg_7d, v.volatility_30d, v.prices_at
FROM lp_offer_values v
JOIN lp_store_corps c ON c.corporation_id = v.corporation_id
WHERE v.region_id = sqlc.arg(region_id)
  AND (sqlc.arg(corporation_id)::bigint = 0 OR v.corporation_id = sqlc.arg(corporation_id)::bigint)
  AND v.unpriced_reason = ''
  AND v.daily_volume >= sqlc.arg(min_volume)::double precision
  AND (CASE WHEN sqlc.arg(instant)::bool THEN v.isk_per_lp_buy ELSE v.isk_per_lp_sell END) IS NOT NULL
ORDER BY (CASE WHEN sqlc.arg(instant)::bool THEN v.isk_per_lp_buy ELSE v.isk_per_lp_sell END) DESC, v.type_id, v.corporation_id
LIMIT sqlc.arg(row_cap)::bigint;

-- name: CountLPOfferValuesUnpriced :one
-- How many of the region's offers (in the chosen store) stand
-- unpriced, for the page's honest-gap line.
SELECT COUNT(*) FROM lp_offer_values
WHERE region_id = sqlc.arg(region_id)
  AND (sqlc.arg(corporation_id)::bigint = 0 OR corporation_id = sqlc.arg(corporation_id)::bigint)
  AND unpriced_reason <> '';

-- name: ListLPOfferRequirementsFor :many
SELECT corporation_id, offer_id, type_id, quantity FROM lp_offer_requirements
WHERE corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[])
  AND offer_id = ANY(sqlc.arg(offer_ids)::bigint[])
ORDER BY corporation_id, offer_id, type_id;
