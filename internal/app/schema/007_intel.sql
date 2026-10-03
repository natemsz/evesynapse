-- EveSynapse module sweep, cluster 4 (schema addition 007).
-- Intel support tables. Everything in this cluster is public ESI
-- data (wars, incursions, faction warfare, server status) — no
-- character token is involved, so it can't ride the per-character
-- character_snapshots table without faking a character. These two
-- tables are the worker-warmed global store instead.

-- One payload per public dataset, same cache contract as the
-- per-character snapshots: raw JSON plus the response's Expires
-- stored as cached_until (5-minute fallback when ESI sends none).
CREATE TABLE global_snapshots (
    kind         TEXT PRIMARY KEY, -- status | wars | incursions | fw_systems | fw_stats | factions
    payload      TEXT NOT NULL, -- raw ESI JSON
    fetched_at   TEXT NOT NULL, -- RFC3339
    cached_until TEXT NOT NULL -- RFC3339
);

-- War detail payloads (GET /wars/{war_id}/) behind the war ID
-- list: immutable once the war has finished, slowly changing
-- while it runs. The worker warms them bounded per cycle into
-- this store — the killmail_details/contract_details pattern,
-- minus any character association.
CREATE TABLE war_details (
    war_id     INTEGER PRIMARY KEY,
    payload    TEXT NOT NULL, -- raw ESI JSON
    fetched_at TEXT    NOT NULL -- RFC3339
);
