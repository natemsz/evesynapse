-- ---------------------------------------------------------------------
-- Public corporation & alliance records (schema 026): the
-- wants-style queues behind the public corporation page
-- (/corporation/) and alliance page (/alliance/), mirroring
-- pilot_records (schema 015/016). Viewing a corporation or
-- alliance without a record notes a 'pending' row at viewed
-- priority; the worker fills it from ESI's public endpoints
-- (corporation profile, alliance profile + member-corporation
-- list, names baked in so renders never fetch) and a 404 settles
-- as 'missing'. Ready rows go stale after a week and refresh in
-- the background; the pages keep showing the last known record
-- meanwhile.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS corporation_records (
    corporation_id INTEGER PRIMARY KEY,
    payload        TEXT NOT NULL DEFAULT '',
    state          TEXT NOT NULL DEFAULT 'pending',
    fetched_at     TEXT NOT NULL DEFAULT '',
    priority       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS alliance_records (
    alliance_id INTEGER PRIMARY KEY,
    payload     TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'pending',
    fetched_at  TEXT NOT NULL DEFAULT '',
    priority    INTEGER NOT NULL DEFAULT 0
);
