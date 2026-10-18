-- Schema step 057: abyssal estimate storage.
--
-- What core's scoring pass (abyssal.go) computes for every
-- probed instance, so the appraisal core and the contract
-- pages only ever read finished numbers. abyssal_items rows
-- never expire; these columns are refreshed in place.
--
-- score is NULL until the item's family weights and roll
-- ranges are both known; est_value is NULL whenever the
-- inputs cannot support an ISK figure (an honest gap, not a
-- guess). percentiles is a JSON object keyed by attribute id
-- (as text) holding the direction-adjusted percentile of
-- each rolled attribute; derived_stats is a JSON array of
-- the derived figures (repair per second and kin) with their
-- own percentiles. est_basis says which anchor spoke:
-- 'corpus' (a stat-matched bucket of listings), 'curve'
-- (the score curve over the source module's price, with a
-- bounded family correction), or 'prior' (the source price
-- alone, before any listing of the family was ever seen).

ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS family TEXT NOT NULL DEFAULT '';
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS score DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS percentiles JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS derived_stats JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_value DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_low DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_high DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_basis TEXT NOT NULL DEFAULT '';
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS est_comps BIGINT NOT NULL DEFAULT 0;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS premium_mult DOUBLE PRECISION NOT NULL DEFAULT 1;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS cap_attribute_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS cap_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS recreation_cost DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS recreation_attempts DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS primary_abs DOUBLE PRECISION;
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS scored_at TIMESTAMPTZ;

-- The search filters on family and score; the scoring pass
-- drains by scored_at.
CREATE INDEX IF NOT EXISTS abyssal_items_family_score_idx
    ON abyssal_items (family, score);
CREATE INDEX IF NOT EXISTS abyssal_items_scored_idx
    ON abyssal_items (scored_at);
