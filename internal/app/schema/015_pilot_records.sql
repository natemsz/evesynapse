-- ---------------------------------------------------------------------
-- Public pilot records + item type details (applied by db.go when
-- the tables are absent).
--
-- pilot_records is the wants-style queue behind the public pilot
-- page (/pilot/): viewing a stranger's page notes a 'pending' row,
-- the worker fills it from ESI's public character endpoints
-- (profile + employment history + the assembled corp/alliance/faction
-- names, stored as one payload so renders never fetch), and a 404
-- settles as 'missing'. Ready rows go stale after a week and the
-- worker refreshes them in the background; the page keeps showing
-- the last known record meanwhile.
--
-- type_details warms item descriptions the same way: the item
-- details page notes the type it lacks a description for, and the
-- worker stores the public type payload's description text here.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pilot_records (
    character_id INTEGER PRIMARY KEY,
    payload      TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending',
    fetched_at   TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS type_details (
    type_id     INTEGER PRIMARY KEY,
    description TEXT NOT NULL DEFAULT '',
    fetched_at  TEXT NOT NULL DEFAULT ''
);
