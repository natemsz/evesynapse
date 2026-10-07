-- EveSynapse schema_pg/003: fitting metadata (public fits foundation).
-- is_public gates the community-fit search on the editor's "Your
-- fits" bar; is_draft marks the editor's single autosave draft per
-- user (autosave upserts it instead of spawning duplicates).
-- Description and tags ride inside items_json (the fit document),
-- so they need no columns. Applied by openDB wherever the
-- is_public column is absent: fresh installs get it right after
-- the baseline, existing installs pick it up on their next boot.
ALTER TABLE local_fittings ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE local_fittings ADD COLUMN is_draft BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS idx_local_fittings_public ON local_fittings (is_public, updated_at DESC) WHERE is_public;
CREATE INDEX IF NOT EXISTS idx_local_fittings_draft ON local_fittings (user_id, is_draft) WHERE is_draft;
