-- Schema step 149: a variation row's own tier label.
--
-- Implant and booster ladders (hardwiring levels, set grades, booster
-- strengths) join the variation families; their steps carry labels
-- such as Level 3 or Mid-grade in place of the meta group name.
ALTER TABLE sde_variations ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT '';
