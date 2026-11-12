-- Schema step 150: the appraisal tier behind each stored kill
-- line price.
--
-- A kill's totals stay permanent, but while its line prices are
-- retained the page needs to know whether any line was priced at
-- the build-cost tier (the house order's last resort, my
-- ruling 2026-11-12) so the value can be asterisked and labeled.
ALTER TABLE kill_line_prices ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT '';
