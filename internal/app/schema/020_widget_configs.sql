-- Migration 020 (v0.3.04): per-widget configuration. The home
-- layout (schema 010) says WHICH widgets are on and in what
-- order; this table says what each widget is configured to do.
-- Keyed by (user, widget id): a widget id appears at most once
-- in a layout, so the id is the stable instance identity, and
-- configuration survives moves, removal and re-adding. The
-- config itself is a JSON object owned by the widget (the
-- market orders widget stores scope + merge mode); generic
-- storage keeps the next configurable widget free of schema
-- work. Rows vanish with the account.

CREATE TABLE IF NOT EXISTS widget_configs (
    user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    widget_id  TEXT    NOT NULL,
    config     TEXT    NOT NULL DEFAULT '{}', -- JSON object, widget-owned
    updated_at TEXT    NOT NULL,              -- RFC3339
    PRIMARY KEY (user_id, widget_id)
);
