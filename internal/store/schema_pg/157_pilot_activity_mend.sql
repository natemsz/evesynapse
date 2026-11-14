-- The pilot activity mend (pilot_activity_mend.go): one walk per
-- profile over the stored kills it took part in, folding the ones the
-- live stream never folded in (it skipped attackers other than the
-- final blow, and while the history backfill was switched off it
-- folded nothing). The cursor is the last kill the walk folded;
-- mended_at marks a finished walk.
ALTER TABLE pilot_activity_profiles
    ADD COLUMN IF NOT EXISTS mend_cursor_time TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS mend_cursor_id   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS mended_at        TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS pilot_activity_profiles_mend_idx
    ON pilot_activity_profiles (last_active_at DESC NULLS LAST) WHERE mended_at IS NULL;
