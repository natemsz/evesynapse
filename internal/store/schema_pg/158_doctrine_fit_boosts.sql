-- The fleet boosts a doctrine fit gives (doctrines.go): each burst
-- charge it loads with its hull and mindlink, as fit boost JSON, so
-- the fitting tool can fly a fit inside the doctrine's boosts.
-- Written when the fit is added; NULL for fits added before this
-- step, which are worked out the first time they are read.
ALTER TABLE doctrine_fits ADD COLUMN IF NOT EXISTS boosts_json TEXT;
