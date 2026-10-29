-- LP earnings per store (schema 114): balance samples, inferred
-- purchases, what their units sold for, and the per-store summary.

-- name: ListLatestLPSamples :many
SELECT DISTINCT ON (corporation_id) corporation_id, sampled_at, points
FROM lp_balance_samples WHERE character_id = $1
ORDER BY corporation_id, sampled_at DESC;

-- name: InsertLPSample :exec
INSERT INTO lp_balance_samples (character_id, corporation_id, sampled_at, points)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: SumLPStorePaymentsBetween :one
-- The journal's LP store payments inside a purchase's bracket.
SELECT COALESCE(SUM(-amount), 0)::float8 AS isk, COUNT(*)::bigint AS n
FROM ledger_journal
WHERE owner_kind = 'char' AND owner_id = $1 AND ref_type = 'lp_store'
  AND at > sqlc.arg(from_at)::timestamptz AND at <= sqlc.arg(to_at)::timestamptz;

-- name: ListLPOffersForCorp :many
SELECT offer_id, type_id, quantity, lp_cost, isk_cost FROM lp_offers
WHERE corporation_id = $1 AND lp_cost > 0
ORDER BY offer_id;

-- name: ListLPOfferRequirementsForOffer :many
SELECT type_id, quantity FROM lp_offer_requirements
WHERE corporation_id = $1 AND offer_id = $2;

-- name: InsertLPPurchase :exec
INSERT INTO lp_purchases (character_id, corporation_id, from_at, to_at, lp_spent, isk_paid, tier, offer_id, quantity,
                          product_type_id, units, req_value, candidates, journal_isk, assets_before, assets_as_of)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (character_id, corporation_id, to_at) DO NOTHING;

-- name: ListUncheckedLPPurchases :many
-- Inferred purchases waiting on the timed asset check.
SELECT id, product_type_id, units, assets_before, assets_as_of FROM lp_purchases
WHERE character_id = $1 AND tier = 'inferred' AND NOT checked AND product_type_id > 0;

-- name: SetLPPurchaseChecked :exec
UPDATE lp_purchases SET checked = true, tier = $2 WHERE id = $1;

-- name: ListLPPurchasesToPick :many
-- A pool's purchases the player can settle: several offers fit, or
-- none did.
SELECT p.id, p.character_id, p.corporation_id, p.from_at, p.to_at, p.lp_spent, p.isk_paid, p.tier, p.candidates
FROM lp_purchases p
WHERE p.character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND p.tier IN ('ambiguous', 'unexplained')
ORDER BY p.to_at DESC
LIMIT 50;

-- name: GetLPPurchase :one
SELECT * FROM lp_purchases WHERE id = $1 AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: ConfirmLPPurchase :exec
UPDATE lp_purchases SET tier = 'user_confirmed', offer_id = $2, quantity = $3, product_type_id = $4, units = $5,
       isk_paid = $6, req_value = $7, checked = true
WHERE id = $1;

-- name: DeleteLPSaleLinksFrom :exec
DELETE FROM lp_sale_links WHERE pool_kind = $1 AND pool_id = $2 AND type_id = $3 AND at >= sqlc.arg(from_at)::timestamptz;

-- name: DeleteLPSaleLinksForPool :exec
DELETE FROM lp_sale_links WHERE pool_kind = $1 AND pool_id = $2;

-- name: InsertLPSaleLinksBatch :exec
INSERT INTO lp_sale_links (pool_kind, pool_id, purchase_id, type_id, owner_id, transaction_id, at, units, revenue, cost)
SELECT sqlc.arg(pool_kind)::text, sqlc.arg(pool_id)::bigint, unnest(sqlc.arg(purchase_ids)::bigint[]), sqlc.arg(type_id)::bigint,
       unnest(sqlc.arg(owner_ids)::bigint[]), unnest(sqlc.arg(transaction_ids)::bigint[]),
       unnest(sqlc.arg(ats)::text[])::timestamptz, unnest(sqlc.arg(units)::bigint[]),
       unnest(sqlc.arg(revenues)::float8[]), unnest(sqlc.arg(costs)::float8[])
ON CONFLICT (pool_kind, pool_id, purchase_id, transaction_id) DO UPDATE SET
    units = lp_sale_links.units + excluded.units, revenue = lp_sale_links.revenue + excluded.revenue,
    cost = lp_sale_links.cost + excluded.cost;

-- name: FoldProfitLP :exec
-- A character's LP rows from a day on: the ISK of LP store items it
-- sold (at an LP cost of 0: revenue less the ISK and required items
-- they cost), their sales tax moved from trading to LP, and the day's
-- net LP (earned less spent) on the 'lp' row. Runs after
-- FoldProfitDaily, which leaves lp_delta at 0.
INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs, lp_delta)
SELECT 'char', sqlc.arg(character_id)::bigint, day, activity, SUM(income), SUM(costs), SUM(lp)::bigint
FROM (
    SELECT to_char(l.at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, 'lp' AS activity, l.revenue AS income,
           l.cost + COALESCE(s.tax * l.units / NULLIF(t.quantity, 0), 0) AS costs, 0 AS lp
    FROM lp_sale_links l
    JOIN ledger_txns t ON t.owner_kind = 'char' AND t.owner_id = l.owner_id AND t.transaction_id = l.transaction_id
    LEFT JOIN ledger_sale_tax s ON s.owner_kind = 'char' AND s.owner_id = l.owner_id AND s.transaction_id = l.transaction_id
    WHERE l.owner_id = sqlc.arg(character_id) AND l.at >= sqlc.arg(from_day)::date::timestamptz
    UNION ALL
    SELECT to_char(l.at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'trading', 0, -COALESCE(s.tax * l.units / NULLIF(t.quantity, 0), 0), 0
    FROM lp_sale_links l
    JOIN ledger_txns t ON t.owner_kind = 'char' AND t.owner_id = l.owner_id AND t.transaction_id = l.transaction_id
    LEFT JOIN ledger_sale_tax s ON s.owner_kind = 'char' AND s.owner_id = l.owner_id AND s.transaction_id = l.transaction_id
    WHERE l.owner_id = sqlc.arg(character_id) AND l.at >= sqlc.arg(from_day)::date::timestamptz
    UNION ALL
    SELECT to_char(x.sampled_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'lp', 0, 0, x.points - x.prev
    FROM (
        SELECT sampled_at, points, LAG(points) OVER (PARTITION BY corporation_id ORDER BY sampled_at) AS prev
        FROM lp_balance_samples WHERE character_id = sqlc.arg(character_id)
    ) x
    WHERE x.prev IS NOT NULL AND x.sampled_at >= sqlc.arg(from_day)::date::timestamptz
) y
GROUP BY day, activity
ON CONFLICT (owner_kind, owner_id, day, activity) DO UPDATE SET
    income = profit_daily.income + excluded.income, costs = profit_daily.costs + excluded.costs,
    lp_delta = excluded.lp_delta;

-- name: DeleteLPStoreSummaryForPool :exec
DELETE FROM lp_store_summary WHERE pool_kind = $1 AND pool_id = $2;

-- name: FoldLPStoreSummary :exec
-- Per store across a pool's characters: LP earned (balance rises),
-- LP spent by tier, ISK paid, units acquired, and what the sold units
-- brought against what they cost (ISK and required items, LP at 0)
-- with the LP behind them.
INSERT INTO lp_store_summary (pool_kind, pool_id, corporation_id, lp_earned, lp_corroborated, lp_inferred, lp_ambiguous,
    lp_unexplained, isk_paid, units_acquired, units_sold, revenue, tax, sold_cost, lp_sold, folded_at)
SELECT sqlc.arg(pool_kind)::text, sqlc.arg(pool_id)::bigint, c.corporation_id,
       COALESCE(e.earned, 0), COALESCE(p.corroborated, 0), COALESCE(p.inferred, 0), COALESCE(p.ambiguous, 0),
       COALESCE(p.unexplained, 0), COALESCE(p.isk, 0), COALESCE(p.units, 0),
       COALESCE(s.units, 0), COALESCE(s.revenue, 0), COALESCE(s.tax, 0), COALESCE(s.cost, 0), COALESCE(s.lp, 0),
       sqlc.arg(folded_at)::timestamptz
FROM (
    SELECT DISTINCT corporation_id FROM lp_balance_samples WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])
    UNION
    SELECT DISTINCT corporation_id FROM lp_purchases WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])
) c
LEFT JOIN (
    SELECT corporation_id, SUM(GREATEST(points - prev, 0))::bigint AS earned
    FROM (SELECT corporation_id, points, LAG(points) OVER (PARTITION BY character_id, corporation_id ORDER BY sampled_at) AS prev
          FROM lp_balance_samples WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])) x
    WHERE prev IS NOT NULL
    GROUP BY corporation_id
) e ON e.corporation_id = c.corporation_id
LEFT JOIN (
    SELECT corporation_id,
           SUM(CASE WHEN tier IN ('corroborated', 'user_confirmed') THEN lp_spent ELSE 0 END)::bigint AS corroborated,
           SUM(CASE WHEN tier = 'inferred' THEN lp_spent ELSE 0 END)::bigint AS inferred,
           SUM(CASE WHEN tier = 'ambiguous' THEN lp_spent ELSE 0 END)::bigint AS ambiguous,
           SUM(CASE WHEN tier = 'unexplained' THEN lp_spent ELSE 0 END)::bigint AS unexplained,
           SUM(isk_paid)::float8 AS isk,
           SUM(CASE WHEN tier IN ('corroborated', 'inferred', 'user_confirmed') THEN units ELSE 0 END)::bigint AS units
    FROM lp_purchases WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])
    GROUP BY corporation_id
) p ON p.corporation_id = c.corporation_id
LEFT JOIN (
    SELECT pu.corporation_id, SUM(l.units)::bigint AS units, SUM(l.revenue)::float8 AS revenue, SUM(l.cost)::float8 AS cost,
           SUM(COALESCE(st.tax * l.units / NULLIF(t.quantity, 0), 0))::float8 AS tax,
           SUM(l.units::float8 * pu.lp_spent / NULLIF(pu.units, 0))::float8 AS lp
    FROM lp_sale_links l
    JOIN lp_purchases pu ON pu.id = l.purchase_id
    LEFT JOIN ledger_txns t ON t.owner_kind = 'char' AND t.owner_id = l.owner_id AND t.transaction_id = l.transaction_id
    LEFT JOIN ledger_sale_tax st ON st.owner_kind = 'char' AND st.owner_id = l.owner_id AND st.transaction_id = l.transaction_id
    WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id)
    GROUP BY pu.corporation_id
) s ON s.corporation_id = c.corporation_id;

-- name: ListLPUnsoldByCorp :many
-- Units of each store's lot purchases not yet sold, per product type:
-- marked at today's stat pack after the fold.
SELECT p.corporation_id, p.product_type_id, (SUM(p.units) - COALESCE(MAX(s.sold), 0))::bigint AS unsold
FROM lp_purchases p
LEFT JOIN (
    SELECT pu.corporation_id, pu.product_type_id, SUM(l.units) AS sold
    FROM lp_sale_links l JOIN lp_purchases pu ON pu.id = l.purchase_id
    WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id)
    GROUP BY pu.corporation_id, pu.product_type_id
) s ON s.corporation_id = p.corporation_id AND s.product_type_id = p.product_type_id
WHERE p.character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND p.tier IN ('corroborated', 'inferred', 'user_confirmed')
  AND p.product_type_id > 0
GROUP BY p.corporation_id, p.product_type_id;

-- name: SetLPStoreUnsold :exec
UPDATE lp_store_summary SET unsold_value = $4, unsold_priced = $5
WHERE pool_kind = $1 AND pool_id = $2 AND corporation_id = $3;

-- name: ListLPStoreSummary :many
SELECT * FROM lp_store_summary
WHERE pool_kind = sqlc.arg(pool_kind) AND pool_id = ANY(sqlc.arg(pool_ids)::bigint[])
ORDER BY revenue - tax - sold_cost DESC, lp_earned DESC, corporation_id
LIMIT 100;

-- name: GetBestLPOfferValue :one
-- The store's best current ISK per LP selling into sell orders, from
-- the stored offer values in a region.
SELECT COALESCE(MAX(isk_per_lp_sell), 0)::float8 AS isk_per_lp, MAX(updated_at)::timestamptz AS as_of
FROM lp_offer_values WHERE region_id = $1 AND corporation_id = $2;

-- name: SetUserLPValue :exec
INSERT INTO user_profit_prefs (user_id, lp_isk_value, updated_at)
VALUES ($1, $2, now())
ON CONFLICT (user_id) DO UPDATE SET lp_isk_value = excluded.lp_isk_value, updated_at = excluded.updated_at;

-- name: FoldProfitLPDelta :exec
-- Just the day's net LP on a character's 'lp' rows from a day on,
-- after new balance samples with nothing else to fold.
INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs, lp_delta)
SELECT 'char', sqlc.arg(character_id)::bigint, to_char(x.sampled_at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'lp', 0, 0,
       SUM(x.points - x.prev)::bigint
FROM (
    SELECT sampled_at, points, LAG(points) OVER (PARTITION BY corporation_id ORDER BY sampled_at) AS prev
    FROM lp_balance_samples WHERE character_id = sqlc.arg(character_id)
) x
WHERE x.prev IS NOT NULL AND x.sampled_at >= sqlc.arg(from_day)::date::timestamptz
GROUP BY 3
ON CONFLICT (owner_kind, owner_id, day, activity) DO UPDATE SET lp_delta = excluded.lp_delta;

-- name: SumProfitLPDelta :one
-- Net LP over a span, for profit at the player's ISK per LP.
SELECT COALESCE(SUM(lp_delta), 0)::bigint FROM profit_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND day >= sqlc.arg(from_day)::text AND activity = 'lp';
