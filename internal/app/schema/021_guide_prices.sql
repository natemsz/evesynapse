-- Migration 021 (v0.3.04): the stored market guide. The worker
-- keeps GET /markets/prices/ (one global call, ESI's own cache
-- window) mirrored here so asset valuation always has prices to
-- work with — until now the guide lived only in memory, warmed
-- by Market page visits, so the Home net-worth card could sit
-- on "prices not loaded yet" indefinitely. guide_prices_meta
-- carries the refresh bookkeeping (when the table was written,
-- and until when ESI says it stays fresh). The worker replaces
-- the price rows wholesale on each refresh: the endpoint
-- returns the whole universe, so vanished types vanish here.

CREATE TABLE IF NOT EXISTS guide_prices (
    type_id        INTEGER PRIMARY KEY, -- EVE type ID
    adjusted_price REAL NOT NULL DEFAULT 0,
    average_price  REAL NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS guide_prices_meta (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    fetched_at   TEXT NOT NULL DEFAULT '', -- RFC3339
    cached_until TEXT NOT NULL DEFAULT ''  -- RFC3339, from ESI Expires
);
