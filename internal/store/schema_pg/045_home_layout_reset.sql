-- Schema step 045: every saved home layout goes back to the default,
-- once.
--
-- The overview moved to a three-column grid where every module has
-- its own width and height. Layouts saved for the old grid would
-- keep their old order and sizes, so each account starts over on the
-- new default; an empty layout is the default (home_overview.go).
-- Runs once per database like every step, so layouts saved after it
-- stay.

UPDATE users SET home_layout = '' WHERE home_layout <> '';
