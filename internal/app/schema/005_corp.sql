-- EveSynapse module sweep, cluster 2 (schema addition 005).
-- Corporation support tables behind the corporation subpages.
-- Corporation payloads themselves ride the existing
-- character_snapshots table (per viewing character, corp_* kinds);
-- these tables hold what snapshots can't say.

-- Per-(character, kind) fetch outcome for corporation endpoints.
-- Role-gated ESI endpoints answer 403 when the viewing character
-- lacks the in-game role; the worker records that here (state
-- "role_missing", detail = the role label) instead of retrying
-- every cycle, and pages/Sync render a plain "needs the role"
-- state from it. "error" keeps the last failure message. A row is
-- only an outcome log — snapshots stay the data of record, so a
-- 403 never poisons the snapshot cache.
CREATE TABLE snapshot_fetch_state (
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    kind         TEXT    NOT NULL, -- snapshot kind, or a worker pseudo-kind (corp_asset_names)
    state        TEXT    NOT NULL, -- "ok" | "role_missing" | "error"
    detail       TEXT    NOT NULL DEFAULT '', -- role label or error text
    attempted_at TEXT    NOT NULL, -- RFC3339
    PRIMARY KEY (character_id, kind)
);

-- Which corporation each character belongs to, resolved by the
-- worker from the public character sheet. Handlers read this
-- (never ESI) to scope corporation pages and to tell a corp kill
-- (victim in another corp) from a corp loss (victim in this one).
-- Keyed by character on purpose: character and corporation IDs
-- share EVE's numeric space, so corp data is always reached
-- *through* a character row, never by a bare corporation ID.
CREATE TABLE character_corporations (
    character_id   INTEGER PRIMARY KEY REFERENCES characters (character_id) ON DELETE CASCADE,
    corporation_id INTEGER NOT NULL,
    updated_at     TEXT    NOT NULL -- RFC3339
);

-- Player-given names for singleton items (named ships, renamed
-- containers), warmed from POST /corporations/{id}/assets/names/.
-- Item IDs are globally unique in EVE, so one flat table serves
-- every viewer.
CREATE TABLE item_names (
    item_id INTEGER PRIMARY KEY, -- EVE item ID
    name    TEXT    NOT NULL
);
