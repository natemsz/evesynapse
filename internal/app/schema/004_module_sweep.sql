-- EveSynapse module sweep (schema addition 004).
-- Killmail detail payloads warmed by the worker from the recent-
-- killmails list: details are immutable once posted, so they are
-- stored once per killmail and never expire (fetched_at is kept
-- for diagnostics only). The recent list itself stays a snapshot
-- (kind "killmails"); this table is only the detail store behind it.

CREATE TABLE killmail_details (
    killmail_id  INTEGER PRIMARY KEY, -- CCP killmail ID
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    hash         TEXT    NOT NULL,    -- the hash the detail was fetched with
    payload      TEXT    NOT NULL,    -- raw ESI JSON body of GET /killmails/{id}/{hash}/
    fetched_at   TEXT    NOT NULL     -- RFC3339
);
