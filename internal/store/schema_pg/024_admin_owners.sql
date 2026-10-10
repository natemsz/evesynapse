-- Schema step 024: who owned each administrator character when it was
-- first recognised as one. Administrator status holds only while the
-- character's EVE owner hash is still that one, so a character that is
-- sold or transferred does not carry the status to its new owner.

CREATE TABLE IF NOT EXISTS admin_owners (
    character_id BIGINT PRIMARY KEY,
    owner_hash   TEXT NOT NULL,
    pinned_at    timestamptz NOT NULL DEFAULT now()
);
