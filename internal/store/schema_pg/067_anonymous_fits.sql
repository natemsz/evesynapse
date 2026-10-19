-- Schema step 067: anonymous fit sharing.
--
-- An anonymous fit is a local_fittings row owned by a shared
-- anonymous owner account, carrying an unguessable share
-- token and is_anonymous. It is link-only: is_public stays
-- false, so it never appears in the public fit browser. It
-- falls off 180 days after its last view, like anonymous
-- appraisals. The signing secret for the creator's history
-- cookie lives beside the appraisal one, in its own table.

ALTER TABLE local_fittings
    ADD COLUMN IF NOT EXISTS share_token TEXT NOT NULL DEFAULT '';
ALTER TABLE local_fittings
    ADD COLUMN IF NOT EXISTS is_anonymous BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE local_fittings
    ADD COLUMN IF NOT EXISTS last_viewed_at TIMESTAMPTZ;

CREATE UNIQUE INDEX IF NOT EXISTS local_fittings_share_token_idx
    ON local_fittings (share_token)
    WHERE share_token <> '';
CREATE INDEX IF NOT EXISTS local_fittings_anon_expiry_idx
    ON local_fittings (last_viewed_at)
    WHERE is_anonymous;

CREATE TABLE IF NOT EXISTS fitting_anonymous_owner (
    id      BIGINT PRIMARY KEY CHECK (id = 1),
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS fitting_cookie_secrets (
    id         BIGINT PRIMARY KEY CHECK (id = 1),
    secret     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
