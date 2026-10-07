-- Schema step 008: the ETag of the ESI response each snapshot was
-- stored from ('' when there is none: paginated datasets, and
-- everything stored before this step).
--
-- With it the next refresh can ask ESI "has this changed?" instead
-- of downloading the dataset again: an unchanged one comes back as
-- 304 Not Modified with no body, and only the cache window is
-- renewed.

ALTER TABLE character_snapshots ADD COLUMN IF NOT EXISTS etag TEXT NOT NULL DEFAULT '';
ALTER TABLE global_snapshots ADD COLUMN IF NOT EXISTS etag TEXT NOT NULL DEFAULT '';
