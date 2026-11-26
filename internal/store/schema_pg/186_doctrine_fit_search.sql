-- Schema step 186: a doctrine fit's search text, built when it is saved.
--
-- The loadouts browser searches every fit of an owner by its module
-- and charge names and filters by its own tags. Those come from the
-- fit's items_json and the type names, so they are worked out once,
-- when the fit is added (and by a worker pass for fits stored before
-- this step), never on a page view. NULL means not worked out yet.

ALTER TABLE doctrine_fits
    ADD COLUMN IF NOT EXISTS search_text TEXT,
    ADD COLUMN IF NOT EXISTS fit_tags TEXT;
