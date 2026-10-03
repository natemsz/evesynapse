-- EveSynapse initial schema (SQLite).
-- Managed as sqlc schema input; the running app creates the scs
-- `sessions` table itself, everything else lives here.

CREATE TABLE users (
    id         INTEGER PRIMARY KEY,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE characters (
    character_id  INTEGER PRIMARY KEY, -- CCP character ID
    user_id       INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          TEXT    NOT NULL DEFAULT '',
    access_token  TEXT    NOT NULL DEFAULT '',
    refresh_token TEXT    NOT NULL DEFAULT '',
    token_expiry  TEXT,                -- RFC3339, when access_token dies
    scopes        TEXT    NOT NULL DEFAULT '', -- space-separated ESI scopes
    cached_until  TEXT,                -- RFC3339, ESI cache expiry for the sheet
    created_at    TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_characters_user_id ON characters (user_id);
