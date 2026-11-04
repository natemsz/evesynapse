-- Schema step 122: can each owned blueprint be built right now.
--
-- One row per blueprint an account's characters own (by item),
-- folded by the worker from the characters' stored blueprint, asset
-- and skill snapshots whenever one of them advances or the account's
-- characters change. Two verdicts per blueprint:
--
--   * the owner's: the owning character has every manufacturing skill
--     the blueprint asks for, and one run's direct materials at the
--     blueprint's ME sit in that character's own hangars;
--   * the account's: some character on the account has the skills,
--     and the materials sit across all the account's hangars.
--
-- The Blueprints page reads the owner's verdict for one character and
-- the account's for all characters; it never derives either.

CREATE TABLE IF NOT EXISTS blueprint_buildable (
    user_id             BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character_id        BIGINT NOT NULL REFERENCES characters (character_id) ON DELETE CASCADE,
    item_id             BIGINT NOT NULL,
    blueprint_type      BIGINT NOT NULL,
    total_materials     BIGINT NOT NULL DEFAULT 0,
    has_skills          BOOLEAN NOT NULL,
    has_materials       BOOLEAN NOT NULL,
    missing_skills      TEXT NOT NULL DEFAULT '',
    short_materials     BIGINT NOT NULL DEFAULT 0,
    any_skills          BOOLEAN NOT NULL,
    any_materials       BOOLEAN NOT NULL,
    any_missing_skills  TEXT NOT NULL DEFAULT '',
    any_short_materials BIGINT NOT NULL DEFAULT 0,
    computed_at         TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (character_id, item_id)
);
CREATE INDEX IF NOT EXISTS blueprint_buildable_user_idx ON blueprint_buildable (user_id, item_id);

-- The last fold per account: which characters it covered (sorted
-- ids, comma-separated) and when, so a linked or unlinked character
-- refolds even when no snapshot advanced.
CREATE TABLE IF NOT EXISTS blueprint_buildable_folds (
    user_id     BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    characters  TEXT NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL
);
