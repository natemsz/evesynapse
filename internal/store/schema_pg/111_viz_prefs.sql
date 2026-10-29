-- Schema step 111: visualization preferences.
--
-- viz_prefs: one account's chart display switches as a JSON
-- object (keys ma_channel, wicks, volume_strip), written from the
-- visualization settings modal and read by the item chart. Empty
-- means the defaults: every switch on. Later chart slices add
-- their keys to the same object.
ALTER TABLE users ADD COLUMN IF NOT EXISTS viz_prefs TEXT NOT NULL DEFAULT '';
