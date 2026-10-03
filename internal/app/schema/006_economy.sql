-- EveSynapse module sweep, cluster 3 (schema addition 006).
-- Contract item detail store behind the Contracts page: the item
-- list behind each contract is immutable once posted, so the
-- worker warms it here once per contract (bounded per cycle) and
-- pages render from this table only. The contracts list itself
-- stays a snapshot (kind "contracts"); this is only the detail
-- store behind it, mirroring killmail_details (schema 004).

CREATE TABLE contract_details (
    contract_id  INTEGER PRIMARY KEY, -- CCP contract ID
    character_id INTEGER NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    payload      TEXT    NOT NULL,    -- raw ESI JSON body of GET /characters/{id}/contracts/{contract_id}/items/
    fetched_at   TEXT    NOT NULL     -- RFC3339
);
