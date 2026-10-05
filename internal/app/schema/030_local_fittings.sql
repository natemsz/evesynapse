-- ---------------------------------------------------------------------
-- Local fittings (schema 030, v0.3.21): fits built in the fitting
-- simulator, stored per user. EveSynapse reads EVE's saved fittings
-- but does not write back to EVE (the fitting write scope is not
-- requested), so fits made here live in this table only. items_json
-- is the fit document: ship type, item lines (type + quantity) and
-- the per-weapon charge choices, as one JSON blob; name and
-- ship_type_id are denormalized for the list view.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS local_fittings (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id      INTEGER NOT NULL,
    name         TEXT    NOT NULL DEFAULT '',
    ship_type_id INTEGER NOT NULL DEFAULT 0,
    items_json   TEXT    NOT NULL DEFAULT '{}',
    created_at   TEXT    NOT NULL DEFAULT '',
    updated_at   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_local_fittings_user ON local_fittings (user_id, updated_at DESC, id DESC);
