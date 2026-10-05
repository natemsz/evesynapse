-- ---------------------------------------------------------------------
-- Order lifecycle (schema 033, P4 own-order archaeology): append-only
-- per-order history distilled from the order snapshots the worker
-- already polls. One row per (character, order): the first time an
-- order is seen it is inserted, every later sighting updates its
-- latest price and remaining volume, and when it vanishes from the
-- open-orders snapshot it is closed as filled (nothing left) or
-- ended (stock left). Closed rows keep the whole story so the
-- Orders page can show fill rates and how often an order was
-- beaten at its own station without ever re-reading a book.
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS order_lifecycle (
    character_id       INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    order_id           INTEGER NOT NULL, -- ESI order_id
    type_id            INTEGER NOT NULL, -- EVE type ID
    location_id        INTEGER NOT NULL, -- station or structure ID
    region_id          INTEGER NOT NULL, -- EVE region ID
    is_buy_order       INTEGER NOT NULL DEFAULT 0, -- 1 = buy, 0 = sell
    listed_price       REAL    NOT NULL DEFAULT 0, -- latest observed price
    volume_total       INTEGER NOT NULL DEFAULT 0, -- units listed
    volume_remain_last INTEGER NOT NULL DEFAULT 0, -- units left at last sighting
    first_seen_at      TEXT    NOT NULL DEFAULT '', -- RFC3339 first observation
    last_seen_at       TEXT    NOT NULL DEFAULT '', -- RFC3339 last observation
    closed_at          TEXT    NOT NULL DEFAULT '', -- RFC3339 close, '' while open
    close_kind         TEXT    NOT NULL DEFAULT '', -- '' while open, 'filled' or 'ended'
    outbid_events      INTEGER NOT NULL DEFAULT 0, -- transitions into being beaten at own station
    beaten_now         INTEGER NOT NULL DEFAULT 0, -- 1 when currently beaten at own station
    PRIMARY KEY (character_id, order_id)
);

CREATE INDEX IF NOT EXISTS idx_order_lifecycle_character_closed ON order_lifecycle (character_id, closed_at);
