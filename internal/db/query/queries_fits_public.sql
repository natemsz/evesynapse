-- ---------------------------------------------------------------------
-- Public discovery: the anonymous fit browser, ratings and sitemap
-- reads. Every browser filter is a SQL predicate and every page is
-- LIMIT/OFFSET bounded; Go never filters full tables. Every sort
-- spans every public fit; the default is newest first. Age is a
-- filter the visitor picks (sincedays, 0 for any time), never a
-- side effect of the sort. The rank sort is Bayesian (fit average
-- shrunk toward the global mean with weight 5, via public_fit_rank
-- in migrations 033 and 041), so one 5-star vote cannot top the
-- chart. Callers pass the global mean as (SELECT public_fit_mean()),
-- which Postgres computes once per query. Meta groups resolve
-- through dogma attribute 1692 (4 faction, 5 officer, 6 deadspace,
-- 2 tech II), the same source the fitting engine reads. The faction
-- filter is Any or No faction modules; a max price excludes only
-- fits with a known price above the cap (0 means unknown). Tags
-- match whole, whatever their case.
-- ---------------------------------------------------------------------

-- name: CountPublicFits :one
SELECT COUNT(*)
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
LEFT JOIN sde_types st ON st.type_id = lf.ship_type_id
LEFT JOIN sde_groups sg ON sg.group_id = st.group_id
WHERE lf.is_public AND NOT lf.is_draft
  AND (sqlc.arg(sincedays)::int <= 0 OR lf.updated_at >= now() - make_interval(days => sqlc.arg(sincedays)::int))
  AND (sqlc.arg(q)::text = '' OR lf.name ILIKE '%' || sqlc.arg(q)::text || '%'
    OR tn.name ILIKE '%' || sqlc.arg(q)::text || '%'
    OR EXISTS (SELECT 1 FROM characters c WHERE c.user_id = lf.user_id AND c.name ILIKE '%' || sqlc.arg(q)::text || '%'))
  AND (sqlc.arg(role)::text = '' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
        WHERE lower(t.tag) = lower(sqlc.arg(role)::text)))
  AND (sqlc.arg(hull)::text = '' OR sg.name = sqlc.arg(hull)::text)
  AND (sqlc.arg(hullq)::text = '' OR tn.name ILIKE '%' || sqlc.arg(hullq)::text || '%')
  AND (sqlc.arg(minstars)::float8 <= 0 OR (lf.rating_count > 0 AND lf.rating_sum::float8 / lf.rating_count >= sqlc.arg(minstars)::float8))
  AND (sqlc.arg(include)::bigint <= 0 OR EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        WHERE (it->>'typeId')::bigint = sqlc.arg(include)::bigint))
  AND (sqlc.arg(exclude)::bigint <= 0 OR NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        WHERE (it->>'typeId')::bigint = sqlc.arg(exclude)::bigint))
  AND (sqlc.arg(faction)::text != 'none' OR NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        JOIN sde_type_attributes ma ON ma.type_id = (it->>'typeId')::bigint AND ma.attribute_id = 1692 AND ma.value IN (4, 5, 6)))
  AND (sqlc.arg(maxprice)::float8 <= 0 OR lf.total_price <= 0 OR lf.total_price <= sqlc.arg(maxprice)::float8)
  AND (sqlc.arg(alpha)::boolean IS NOT TRUE OR (
        sg.name IN ('Corvette', 'Frigate', 'Destroyer', 'Cruiser', 'Battlecruiser', 'Battleship', 'Industrial', 'Mining Frigate', 'Shuttle')
        AND NOT EXISTS (
            SELECT 1 FROM jsonb_array_elements(
                CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
            JOIN sde_type_attributes ma ON ma.type_id = (it->>'typeId')::bigint AND ma.attribute_id = 1692 AND ma.value = 2)))
  AND (sqlc.arg(tag)::text = '' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
        WHERE lower(t.tag) = lower(sqlc.arg(tag)::text)));

-- name: ListPublicFits :many
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       lf.view_count, lf.total_price, lf.rating_count, lf.rating_sum,
       COALESCE(tn.name, '')::text AS ship_name,
       COALESCE(sg.name, '')::text AS hull,
       COALESCE((SELECT c.name FROM characters c WHERE c.user_id = lf.user_id ORDER BY c.character_id LIMIT 1), '')::text AS author_name
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
LEFT JOIN sde_types st ON st.type_id = lf.ship_type_id
LEFT JOIN sde_groups sg ON sg.group_id = st.group_id
WHERE lf.is_public AND NOT lf.is_draft
  AND (sqlc.arg(sincedays)::int <= 0 OR lf.updated_at >= now() - make_interval(days => sqlc.arg(sincedays)::int))
  AND (sqlc.arg(q)::text = '' OR lf.name ILIKE '%' || sqlc.arg(q)::text || '%'
    OR tn.name ILIKE '%' || sqlc.arg(q)::text || '%'
    OR EXISTS (SELECT 1 FROM characters c WHERE c.user_id = lf.user_id AND c.name ILIKE '%' || sqlc.arg(q)::text || '%'))
  AND (sqlc.arg(role)::text = '' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
        WHERE lower(t.tag) = lower(sqlc.arg(role)::text)))
  AND (sqlc.arg(hull)::text = '' OR sg.name = sqlc.arg(hull)::text)
  AND (sqlc.arg(hullq)::text = '' OR tn.name ILIKE '%' || sqlc.arg(hullq)::text || '%')
  AND (sqlc.arg(minstars)::float8 <= 0 OR (lf.rating_count > 0 AND lf.rating_sum::float8 / lf.rating_count >= sqlc.arg(minstars)::float8))
  AND (sqlc.arg(include)::bigint <= 0 OR EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        WHERE (it->>'typeId')::bigint = sqlc.arg(include)::bigint))
  AND (sqlc.arg(exclude)::bigint <= 0 OR NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        WHERE (it->>'typeId')::bigint = sqlc.arg(exclude)::bigint))
  AND (sqlc.arg(faction)::text != 'none' OR NOT EXISTS (
        SELECT 1 FROM jsonb_array_elements(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
        JOIN sde_type_attributes ma ON ma.type_id = (it->>'typeId')::bigint AND ma.attribute_id = 1692 AND ma.value IN (4, 5, 6)))
  AND (sqlc.arg(maxprice)::float8 <= 0 OR lf.total_price <= 0 OR lf.total_price <= sqlc.arg(maxprice)::float8)
  AND (sqlc.arg(alpha)::boolean IS NOT TRUE OR (
        sg.name IN ('Corvette', 'Frigate', 'Destroyer', 'Cruiser', 'Battlecruiser', 'Battleship', 'Industrial', 'Mining Frigate', 'Shuttle')
        AND NOT EXISTS (
            SELECT 1 FROM jsonb_array_elements(
                CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
            JOIN sde_type_attributes ma ON ma.type_id = (it->>'typeId')::bigint AND ma.attribute_id = 1692 AND ma.value = 2)))
  AND (sqlc.arg(tag)::text = '' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
        WHERE lower(t.tag) = lower(sqlc.arg(tag)::text)))
ORDER BY
  CASE WHEN sqlc.arg(sort)::text = 'rank' THEN public_fit_rank(lf.rating_sum, lf.rating_count, (SELECT public_fit_mean())) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'views' THEN lf.view_count END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'new' THEN lf.updated_at END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'old' THEN lf.updated_at END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'name' THEN lf.name END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'namedesc' THEN lf.name END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'pricelo' THEN NULLIF(lf.total_price, 0) END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'pricehi' THEN NULLIF(lf.total_price, 0) END DESC NULLS LAST,
  lf.id DESC
LIMIT sqlc.arg(rlimit)::bigint OFFSET sqlc.arg(roffset)::bigint;

-- name: PublicFitHullFacets :many
SELECT sg.name::text AS hull, COUNT(*)::bigint AS fits
FROM local_fittings lf
JOIN sde_types st ON st.type_id = lf.ship_type_id
JOIN sde_groups sg ON sg.group_id = st.group_id
WHERE lf.is_public AND NOT lf.is_draft
GROUP BY sg.name
ORDER BY COUNT(*) DESC, sg.name
LIMIT 50;

-- name: PublicFitTagFacets :many
SELECT MIN(t.tag)::text AS tag, COUNT(DISTINCT lf.id)::bigint AS fits
FROM local_fittings lf,
  jsonb_array_elements_text(
    CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
WHERE lf.is_public AND NOT lf.is_draft
GROUP BY lower(t.tag)
ORDER BY COUNT(DISTINCT lf.id) DESC, MIN(t.tag)
LIMIT 30;

-- name: GetFitRating :one
SELECT fit_id, user_id, stars FROM fit_ratings
WHERE fit_id = $1 AND user_id = $2;

-- name: UpsertFitRating :exec
INSERT INTO fit_ratings (fit_id, user_id, stars, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (fit_id, user_id) DO UPDATE SET stars = EXCLUDED.stars, updated_at = now();

-- LockPublicFitForRating takes the fit's row for the rest of the
-- transaction, so two ratings of one fit run one after the other.
-- name: LockPublicFitForRating :one
SELECT id FROM local_fittings
WHERE id = $1 AND is_public AND NOT is_draft
FOR UPDATE;

-- RecountFitRatings sets a fit's aggregates to what fit_ratings
-- holds. They are never moved by a delta: a delta is only right if
-- the read behind it is still true when it lands.
-- name: RecountFitRatings :exec
UPDATE local_fittings
SET rating_count = (SELECT COUNT(*) FROM fit_ratings r WHERE r.fit_id = $1),
    rating_sum = (SELECT COALESCE(SUM(r.stars), 0) FROM fit_ratings r WHERE r.fit_id = $1)
WHERE id = $1;

-- name: BumpFitViewCount :exec
UPDATE local_fittings SET view_count = view_count + 1 WHERE id = $1;

-- name: SetLocalFittingPrice :exec
UPDATE local_fittings SET total_price = $1 WHERE id = $2 AND user_id = $3;

-- RepriceLocalFittings sets every fit's total_price from the guide
-- mirror: the hull plus each module line at adjusted price, the sum
-- fitDocPrice makes when a fit is saved (0 when nothing in the fit
-- is priced). Rows whose price is unchanged are not written, and an
-- empty guide prices nothing. updated_at is left alone: a new price
-- is not an edit.
-- name: RepriceLocalFittings :execrows
UPDATE local_fittings lf
SET total_price = priced.total
FROM (
  SELECT f.id,
         (COALESCE((SELECT gp.adjusted_price FROM guide_prices gp WHERE gp.type_id = f.ship_type_id), 0) +
          COALESCE((SELECT SUM((it->>'qty')::numeric * gp.adjusted_price)
                    FROM jsonb_array_elements(
                        CASE WHEN jsonb_typeof(f.items_json::jsonb -> 'items') = 'array' THEN f.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
                    JOIN guide_prices gp ON gp.type_id = (it->>'typeId')::bigint), 0))::float8 AS total
  FROM local_fittings f
) priced
WHERE priced.id = lf.id
  AND lf.total_price IS DISTINCT FROM priced.total
  AND EXISTS (SELECT 1 FROM guide_prices);

-- name: SitemapTopFits :many
SELECT lf.id, lf.updated_at
FROM local_fittings lf
WHERE lf.is_public AND NOT lf.is_draft
ORDER BY public_fit_rank(lf.rating_sum, lf.rating_count, (SELECT public_fit_mean())) DESC,
         lf.id DESC
LIMIT 1000;

-- name: SitemapTopItems :many
SELECT type_id, SUM(avg_daily_volume)::float8 AS volume
FROM market_region_stats
GROUP BY type_id
ORDER BY SUM(avg_daily_volume) DESC
LIMIT 1000;

-- name: SitemapTopShips :many
SELECT ship_type_id, COUNT(*)::bigint AS fits
FROM local_fittings
WHERE is_public AND NOT is_draft AND ship_type_id > 0
GROUP BY ship_type_id
ORDER BY COUNT(*) DESC, ship_type_id
LIMIT 200;

-- name: ListPublicHubStats :many
SELECT m.region_id, m.best_sell, m.best_buy,
       COALESCE(r.name, '')::text AS region_name
FROM market_region_stats m
LEFT JOIN sde_regions r ON r.region_id = m.region_id
WHERE m.type_id = $1
ORDER BY m.region_id;

-- name: ListPublicFitsForShip :many
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       lf.view_count, lf.total_price, lf.rating_count, lf.rating_sum,
       COALESCE(tn.name, '')::text AS ship_name,
       COALESCE(sg.name, '')::text AS hull,
       COALESCE((SELECT c.name FROM characters c WHERE c.user_id = lf.user_id ORDER BY c.character_id LIMIT 1), '')::text AS author_name
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
LEFT JOIN sde_types st ON st.type_id = lf.ship_type_id
LEFT JOIN sde_groups sg ON sg.group_id = st.group_id
WHERE lf.is_public AND NOT lf.is_draft AND lf.ship_type_id = $1
ORDER BY public_fit_rank(lf.rating_sum, lf.rating_count, (SELECT public_fit_mean())) DESC,
         lf.id DESC
LIMIT $2;
