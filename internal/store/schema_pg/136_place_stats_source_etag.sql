-- Schema step 136: the source version behind each place-stats set.
--
-- Place stats distilled from a stored global snapshot record the
-- snapshot's ETag, so an unchanged ESI answer (a 304 that only
-- renews the snapshot's window) does not rewrite the set. Empty
-- for sets fetched directly.
ALTER TABLE place_stats_meta ADD COLUMN IF NOT EXISTS source_etag TEXT NOT NULL DEFAULT '';
