-- Migration 017 (Phase 6): the Briefing home module's window
-- anchor — when this account last saw its home briefing, so the
-- digest can cover "since you last looked". '' means never.
ALTER TABLE users ADD COLUMN last_briefing_at TEXT NOT NULL DEFAULT '';
