-- Schema step 040: recount every fit's rating aggregates.
--
-- rating_count and rating_sum on a fit were moved by a delta the
-- handler worked out from a read made before its write. Two posts
-- of one account's first rating (a double click) both read "no
-- rating yet" and both added one, so the fit counted a rating that
-- fit_ratings holds once. The handler now recounts from
-- fit_ratings; this brings the fits rated before that into line.

UPDATE local_fittings lf
SET rating_count = agg.n, rating_sum = agg.total
FROM (
  SELECT f.id,
         COUNT(r.user_id)::bigint AS n,
         COALESCE(SUM(r.stars), 0)::bigint AS total
  FROM local_fittings f
  LEFT JOIN fit_ratings r ON r.fit_id = f.id
  GROUP BY f.id
) agg
WHERE agg.id = lf.id
  AND (lf.rating_count <> agg.n OR lf.rating_sum <> agg.total);
