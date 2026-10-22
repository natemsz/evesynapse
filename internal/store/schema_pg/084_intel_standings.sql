-- Intel channels may tell friend from foe by standings too (schema
-- 084): the owner's own contacts, when the owner turns it on. Off, a
-- pilot is friendly only by the channel's own side (intel_analysis.go).
ALTER TABLE intel_channels ADD COLUMN IF NOT EXISTS use_standings BOOLEAN NOT NULL DEFAULT FALSE;
