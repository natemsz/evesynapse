-- Migration 013 (Phase 5): market history + alerts. Three halves:
--
--   1. Price history proper (market_history): ESI's daily
--      aggregates per (region, type), stored as typed rows
--      because charts and change-math aggregate them; the
--      snapshot blob store would mean parsing whole payloads at
--      render. market_history_wants remembers which (region,
--      type) pairs an item-page visit asked for before any rows
--      existed, so the worker can fill them without handlers
--      ever calling ESI. market_fetch_state is the user-agnostic
--      twin of snapshot_fetch_state (which is character-keyed):
--      one row per fetch kind (history_<region>_<type>,
--      book_<region>_<type>) with the last outcome, so history is
--      refetched at most once per 20h and books on their TTL.
--
--   2. The watchlist (market_watchlist): a user's watched types
--      in a region with a movement threshold (percent over 7
--      days). Rows vanish with the account.
--
--   3. Order health (order_health): the worker's computed verdict
--      per open sell order (best price / undercut / cheaper in
--      region), worker-written so /market/, Home and the
--      attention feed render cache-only. Rows vanish with the
--      character and are pruned when orders close.

CREATE TABLE IF NOT EXISTS market_history (
    region_id   INTEGER NOT NULL, -- EVE region ID (10000002 = The Forge)
    type_id     INTEGER NOT NULL, -- EVE type ID
    date        TEXT    NOT NULL, -- "2006-01-02" (ESI day)
    average     REAL    NOT NULL DEFAULT 0, -- daily average price
    highest     REAL    NOT NULL DEFAULT 0,
    lowest      REAL    NOT NULL DEFAULT 0,
    volume      INTEGER NOT NULL DEFAULT 0, -- units traded that day
    order_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (region_id, type_id, date)
);

CREATE TABLE IF NOT EXISTS market_history_wants (
    region_id         INTEGER NOT NULL,
    type_id           INTEGER NOT NULL,
    last_requested_at TEXT    NOT NULL, -- RFC3339
    PRIMARY KEY (region_id, type_id)
);

CREATE TABLE IF NOT EXISTS market_fetch_state (
    kind         TEXT PRIMARY KEY, -- "history_<region>_<type>" | "book_<region>_<type>"
    state        TEXT NOT NULL,    -- "ok" | "error"
    detail       TEXT NOT NULL DEFAULT '',
    attempted_at TEXT NOT NULL     -- RFC3339
);

CREATE TABLE IF NOT EXISTS market_watchlist (
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type_id       INTEGER NOT NULL,
    region_id     INTEGER NOT NULL DEFAULT 10000002,
    threshold_pct REAL    NOT NULL DEFAULT 5, -- move over 7 days that counts as "moving"
    created_at    TEXT    NOT NULL,           -- RFC3339
    PRIMARY KEY (user_id, type_id, region_id)
);

CREATE TABLE IF NOT EXISTS order_health (
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    order_id     INTEGER NOT NULL, -- ESI order_id of the open sell order
    type_id      INTEGER NOT NULL,
    region_id    INTEGER NOT NULL,
    location_id  INTEGER NOT NULL,
    my_price     REAL    NOT NULL,
    station_best REAL    NOT NULL DEFAULT 0, -- cheapest sell at the order's location, 0 = none seen
    region_best  REAL    NOT NULL DEFAULT 0, -- cheapest sell in the region, 0 = none seen
    status       TEXT    NOT NULL, -- "best" | "undercut_station" | "undercut_region" | "best_region_cheaper"
    computed_at  TEXT    NOT NULL, -- RFC3339
    PRIMARY KEY (character_id, order_id)
);
