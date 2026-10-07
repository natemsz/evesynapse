-- Schema step 006 (v0.3.35): custom jump-clone names.
--
-- clone_names: pilot-given labels for jump clones. ESI exposes no
-- custom clone naming, so the character sheet lets the user name
-- each clone; the sheet falls back to "Jump Clone N" when no name
-- is stored. clone_id is the ESI jump_clone_id from the clones
-- snapshot.

CREATE TABLE IF NOT EXISTS clone_names (
    user_id      BIGINT NOT NULL,
    character_id BIGINT NOT NULL,
    clone_id     BIGINT NOT NULL,
    custom_name  TEXT NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, character_id, clone_id)
);
CREATE INDEX IF NOT EXISTS idx_clone_names_character
    ON clone_names (user_id, character_id);
