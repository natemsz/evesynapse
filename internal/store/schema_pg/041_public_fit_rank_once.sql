-- Schema step 041: the public fit rank computes the global mean once
-- per query, not once per row.
--
-- public_fit_rank(sum, count) held its own sub-select over every
-- public fit for the global mean, so ranking n fits read the table n
-- more times. The mean is now its own function, and the rank takes
-- it as an argument: callers pass (SELECT public_fit_mean()), an
-- uncorrelated scalar sub-select Postgres evaluates once per query.
-- The formula is unchanged: the fit's average shrunk toward the
-- global mean with weight 5.

CREATE OR REPLACE FUNCTION public_fit_mean()
RETURNS DOUBLE PRECISION LANGUAGE sql STABLE AS $$
  SELECT COALESCE(AVG(CASE WHEN pf.rating_count > 0 THEN pf.rating_sum::float8 / pf.rating_count END), 3)
  FROM local_fittings pf WHERE pf.is_public AND NOT pf.is_draft
$$;

CREATE OR REPLACE FUNCTION public_fit_rank(rating_sum BIGINT, rating_count BIGINT, mean DOUBLE PRECISION)
RETURNS DOUBLE PRECISION LANGUAGE sql IMMUTABLE AS $$
  SELECT (rating_sum + 5 * mean) / (rating_count + 5)
$$;

DROP FUNCTION IF EXISTS public_fit_rank(BIGINT, BIGINT);
