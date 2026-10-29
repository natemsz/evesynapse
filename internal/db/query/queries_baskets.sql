-- Market baskets (schema 103): presets (user_id NULL) and players'
-- own baskets, their items, and each basket's stored daily value.

-- name: ListMarketBasketsFor :many
-- The basket list: the presets, then the reader's own baskets
-- (user_id 0 reads presets only), each with its latest stored value.
SELECT b.id, b.user_id, b.preset_key, b.name, b.region_id, b.updated_at,
       (SELECT COUNT(*) FROM market_basket_items i WHERE i.basket_id = b.id) AS item_count,
       COALESCE(v.day, '') AS value_day, COALESCE(v.value_sell, 0) AS value_sell,
       COALESCE(v.priced_items, 0) AS priced_items
FROM market_baskets b
LEFT JOIN LATERAL (
    SELECT d.day, d.value_sell, d.priced_items FROM market_basket_values_daily d
    WHERE d.basket_id = b.id ORDER BY d.day DESC LIMIT 1
) v ON true
WHERE b.user_id IS NULL OR b.user_id = sqlc.arg(user_id)::bigint
ORDER BY (b.user_id IS NULL) DESC, b.name, b.id
LIMIT 200;

-- name: GetMarketBasket :one
SELECT id, user_id, preset_key, name, region_id, created_at, updated_at
FROM market_baskets WHERE id = $1;

-- name: CountMarketBasketsForUser :one
SELECT COUNT(*) FROM market_baskets WHERE user_id = $1;

-- name: CreateMarketBasket :one
INSERT INTO market_baskets (user_id, name, region_id)
VALUES ($1, $2, $3)
RETURNING id;

-- name: UpdateMarketBasket :exec
UPDATE market_baskets SET name = $3, region_id = $4, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- name: DeleteMarketBasket :exec
DELETE FROM market_baskets WHERE id = $1 AND user_id = $2;

-- name: DeleteMarketBasketItems :exec
DELETE FROM market_basket_items WHERE basket_id = $1;

-- name: InsertMarketBasketItem :exec
INSERT INTO market_basket_items (basket_id, type_id, quantity)
VALUES ($1, $2, $3)
ON CONFLICT (basket_id, type_id) DO UPDATE SET
    quantity = market_basket_items.quantity + excluded.quantity;

-- name: ListMarketBasketItems :many
SELECT type_id, quantity FROM market_basket_items
WHERE basket_id = $1
ORDER BY type_id
LIMIT 500;

-- name: DeleteMarketBasketValues :exec
DELETE FROM market_basket_values_daily WHERE basket_id = $1;

-- name: RebuildMarketBasketValues :execrows
-- One basket's value history inside the window the pages read
-- (bookTrendDays), from the stat pack's daily rows in the basket's
-- region (the global book's rows for a type that region does not
-- carry). Older values are never read back, so they are not rebuilt.
INSERT INTO market_basket_values_daily (basket_id, day, value_sell, value_buy, priced_items, item_count)
SELECT b.id, d.day,
       SUM(i.quantity * d.typical_sell),
       SUM(i.quantity * d.typical_buy),
       COUNT(*) FILTER (WHERE d.typical_sell > 0),
       (SELECT COUNT(*) FROM market_basket_items c WHERE c.basket_id = b.id)
FROM market_baskets b
JOIN market_basket_items i ON i.basket_id = b.id
JOIN market_region_stats_daily d
  ON d.type_id = i.type_id
 AND (d.region_id = b.region_id
      OR (d.region_id = 19000001 AND NOT EXISTS (
          SELECT 1 FROM market_region_stats_daily dd
          WHERE dd.region_id = b.region_id AND dd.type_id = i.type_id AND dd.day = d.day)))
WHERE b.id = sqlc.arg(basket_id) AND d.day >= sqlc.arg(since)::text
GROUP BY b.id, d.day
ON CONFLICT (basket_id, day) DO UPDATE SET
    value_sell = excluded.value_sell, value_buy = excluded.value_buy,
    priced_items = excluded.priced_items, item_count = excluded.item_count;

-- name: RefreshMarketBasketValuesForRegion :execrows
-- Today's value of every basket priced in one region, from the stat
-- pack rows the region's sweep just wrote (inside its transaction).
INSERT INTO market_basket_values_daily (basket_id, day, value_sell, value_buy, priced_items, item_count)
SELECT b.id, sqlc.arg(day)::text,
       SUM(i.quantity * COALESCE(s.typical_sell, g.typical_sell, 0)),
       SUM(i.quantity * COALESCE(s.typical_buy, g.typical_buy, 0)),
       COUNT(*) FILTER (WHERE COALESCE(s.typical_sell, g.typical_sell, 0) > 0),
       COUNT(*)
FROM market_baskets b
JOIN market_basket_items i ON i.basket_id = b.id
LEFT JOIN market_region_stats s ON s.region_id = b.region_id AND s.type_id = i.type_id
LEFT JOIN market_region_stats g ON g.region_id = 19000001 AND g.type_id = i.type_id AND s.type_id IS NULL
WHERE b.region_id = sqlc.arg(region_id)
GROUP BY b.id
ON CONFLICT (basket_id, day) DO UPDATE SET
    value_sell = excluded.value_sell, value_buy = excluded.value_buy,
    priced_items = excluded.priced_items, item_count = excluded.item_count;

-- name: ListMarketBasketValues :many
-- A basket's value over time, oldest first, from a start day on.
SELECT day, value_sell, value_buy, priced_items, item_count
FROM market_basket_values_daily
WHERE basket_id = sqlc.arg(basket_id) AND day >= sqlc.arg(since)
ORDER BY day
LIMIT 400;

-- name: ListMarketBasketValuesForBaskets :many
-- Recent stored values for a set of baskets at once (the list
-- page's change figures), oldest first per basket.
SELECT basket_id, day, value_sell, value_buy, priced_items, item_count
FROM market_basket_values_daily
WHERE basket_id = ANY(sqlc.arg(basket_ids)::bigint[]) AND day >= sqlc.arg(since)
ORDER BY basket_id, day
LIMIT 2000;

-- name: ListMarketBasketOutsideMovers :many
-- The biggest movers outside a basket: types in the same market
-- groups as its items, in its region, that it does not hold, ranked
-- by the size of their typical-sell change over 1, 7 or 30 days,
-- with the same depth floor the Trends screen uses.
SELECT s.type_id, s.typical_sell, s.sell_chg_1d, s.sell_chg_7d, s.sell_chg_30d, s.avg_daily_volume_30d
FROM market_region_stats s
JOIN sde_types t ON t.type_id = s.type_id
WHERE s.region_id = sqlc.arg(region_id)
  AND t.market_group_id IN (
      SELECT DISTINCT bt.market_group_id FROM market_basket_items bi
      JOIN sde_types bt ON bt.type_id = bi.type_id
      WHERE bi.basket_id = sqlc.arg(basket_id) AND bt.market_group_id > 0)
  AND s.type_id NOT IN (SELECT bi.type_id FROM market_basket_items bi WHERE bi.basket_id = sqlc.arg(basket_id))
  AND s.sell_orders >= 5
  AND (CASE sqlc.arg(period)::int WHEN 1 THEN s.sell_chg_1d WHEN 30 THEN s.sell_chg_30d ELSE s.sell_chg_7d END) IS NOT NULL
ORDER BY ABS(CASE sqlc.arg(period)::int WHEN 1 THEN s.sell_chg_1d WHEN 30 THEN s.sell_chg_30d ELSE s.sell_chg_7d END) DESC, s.type_id
LIMIT 10;

-- name: ResolveTypeNamesExact :many
-- Exact, case-insensitive type names to ids (published market types
-- first) for a pasted basket list.
SELECT DISTINCT ON (lower(name)) type_id, name
FROM sde_types
WHERE lower(name) = ANY(sqlc.arg(names)::text[])
ORDER BY lower(name), (market_group_id > 0) DESC, published DESC, type_id;

-- name: ListMarketBasketsWithoutHistory :many
-- Baskets in a region with no stored value yet (a fresh preset, or
-- one created before its region's first sweep): their history is
-- rebuilt from the daily rows before today's value lands.
SELECT b.id FROM market_baskets b
WHERE b.region_id = $1
  AND EXISTS (SELECT 1 FROM market_basket_items i WHERE i.basket_id = b.id)
  AND NOT EXISTS (SELECT 1 FROM market_basket_values_daily v WHERE v.basket_id = b.id)
ORDER BY b.id
LIMIT 200;

-- name: UpsertWatchlistBasket :exec
INSERT INTO market_watchlist_baskets (user_id, basket_id, threshold_pct)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, basket_id) DO UPDATE SET
    threshold_pct = excluded.threshold_pct;

-- name: DeleteWatchlistBasket :exec
DELETE FROM market_watchlist_baskets
WHERE user_id = $1 AND basket_id = $2;

-- name: ListWatchlistBasketsByUser :many
-- The baskets a player watches that they can still see: their own
-- and the presets. A basket that stops being visible drops off.
SELECT w.basket_id, b.name, b.region_id, w.threshold_pct
FROM market_watchlist_baskets w
JOIN market_baskets b ON b.id = w.basket_id
WHERE w.user_id = $1
  AND (b.user_id IS NULL OR b.user_id = $1)
ORDER BY b.name, w.basket_id;
