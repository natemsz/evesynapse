-- ---------------------------------------------------------------------
-- Schema 022: pilot name-resolution wants behind the top banner
-- search. Typing a character name the local data does not know yet
-- notes the name here (one row per distinct normalized name); the
-- worker resolves it through ESI's public name lookup, queues the
-- pilot record for the resolved character, and the search box
-- turns its "searching" row into the real pilot suggestion once
-- the record lands. A name ESI does not know settles as 'missing'
-- so repeat searches stay quiet; transient failures back off to
-- next_try_at instead of being re-asked on every keystroke.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pilot_name_wants (
    normalized_name TEXT PRIMARY KEY,
    display_name    TEXT NOT NULL DEFAULT '',
    state           TEXT NOT NULL DEFAULT 'pending',
    character_id    INTEGER NOT NULL DEFAULT 0,
    requested_at    TEXT NOT NULL DEFAULT '',
    resolved_at     TEXT NOT NULL DEFAULT '',
    next_try_at     TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0
);
