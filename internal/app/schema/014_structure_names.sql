-- ---------------------------------------------------------------------
-- Player structure names (applied by db.go when the table is
-- absent). Structure ids outlive any single snapshot, and the ESI
-- lookup that names them is authenticated-only, so resolution is a
-- durable queue: 'pending' rows wait for the worker, 'resolved'
-- rows hold the current name, 'missing' rows remember a 403/404
-- (private or destroyed) so private structures aren't re-asked
-- every cycle.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS structure_names (
    structure_id INTEGER PRIMARY KEY,
    name         TEXT NOT NULL DEFAULT '',
    state        TEXT NOT NULL DEFAULT 'pending',
    resolved_at  TEXT NOT NULL DEFAULT ''
);
