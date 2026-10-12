-- Step 033: public discovery counters and ratings. view_count,
-- total_price, rating_count and rating_sum live on the fit row so
-- the public browser sorts and filters without per-render
-- aggregation. fit_ratings holds one row per account per fit.
-- total_price backfills from the stored guide mirror (adjusted
-- ISK per unit, hull plus modules); fits saved afterwards price
-- from the same mirror in the save handler.
ALTER TABLE local_fittings ADD COLUMN IF NOT EXISTS view_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE local_fittings ADD COLUMN IF NOT EXISTS total_price DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE local_fittings ADD COLUMN IF NOT EXISTS rating_count BIGINT NOT NULL DEFAULT 0;
ALTER TABLE local_fittings ADD COLUMN IF NOT EXISTS rating_sum BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS fit_ratings (
  fit_id BIGINT NOT NULL REFERENCES local_fittings (id) ON DELETE CASCADE,
  user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  stars SMALLINT NOT NULL CHECK (stars BETWEEN 1 AND 5),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (fit_id, user_id)
);
CREATE INDEX IF NOT EXISTS idx_fit_ratings_user ON fit_ratings (user_id);
CREATE INDEX IF NOT EXISTS idx_local_fittings_public_browse ON local_fittings (is_public, updated_at DESC, id DESC) WHERE is_public AND NOT is_draft;
UPDATE local_fittings lf SET total_price =
  COALESCE((SELECT gp.adjusted_price FROM guide_prices gp WHERE gp.type_id = lf.ship_type_id), 0) +
  COALESCE((SELECT SUM((it->>'qty')::numeric * COALESCE(gp.adjusted_price, 0))
    FROM jsonb_array_elements(CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'items') = 'array' THEN lf.items_json::jsonb -> 'items' ELSE '[]'::jsonb END) AS it
    JOIN guide_prices gp ON gp.type_id = (it->>'typeId')::bigint), 0)
WHERE lf.total_price = 0;
-- public_fit_rank is the browser's Bayesian rank in one place:
-- the fit's average shrunk toward the global mean with weight 5.
-- The list, ship-page and sitemap queries all call it, so the
-- numbers cannot drift apart.
CREATE OR REPLACE FUNCTION public_fit_rank(rating_sum BIGINT, rating_count BIGINT)
RETURNS DOUBLE PRECISION LANGUAGE sql STABLE AS $$
  SELECT (rating_sum + 5 * (SELECT COALESCE(AVG(CASE WHEN pf.rating_count > 0 THEN pf.rating_sum::float8 / pf.rating_count END), 3)
                            FROM local_fittings pf WHERE pf.is_public AND NOT pf.is_draft)) / (rating_count + 5)
$$;
