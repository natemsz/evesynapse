-- Migration 010 (Phase 1B): the widget-overview home. Layout is one
-- JSON array of widget ids on the user record ('' = no saved layout,
-- use the default). Widgets render cache-only from snapshots, so
-- there is nothing else to store.

ALTER TABLE users ADD COLUMN home_layout TEXT NOT NULL DEFAULT '';
