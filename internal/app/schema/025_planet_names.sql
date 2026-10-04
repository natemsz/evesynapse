-- ---------------------------------------------------------------------
-- Planet names (schema 025): colony planet ids resolve through
-- the public GET /universe/planets/{id}/ — no token needed — but
-- the answer must outlive the process, so resolution is a durable
-- queue like structure names (schema 014): 'pending' rows wait
-- for the worker, 'resolved' rows hold the current name,
-- 'missing' rows remember a 404 (a bad id) so it is not re-asked
-- every cycle. Planet names never change in-game, so resolved
-- rows are trusted for a long window and missing rows back off
-- for days; neither answer is permanent.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS planet_names (
    planet_id   INTEGER PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    state       TEXT NOT NULL DEFAULT 'pending',
    resolved_at TEXT NOT NULL DEFAULT ''
);
