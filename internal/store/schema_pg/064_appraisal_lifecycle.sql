-- Schema step 064: appraisal lifecycle -- anonymous appraisals,
-- creator deletion, and the 180-day fall-off for unviewed
-- anonymous appraisals.
--
-- Anonymous appraisals are stored like signed-in ones, owned by
-- a shared anonymous owner account (created lazily by the app
-- and recorded in appraisal_anonymous_owner), marked with
-- is_anonymous. last_viewed_at drives the expiry sweep: an
-- anonymous appraisal is deleted once it has not been viewed
-- for 180 days. Signed-in appraisals never expire.
--
-- appraisal_cookie_secrets holds the one per-install secret
-- that signs the anonymous-history cookie, so a history cookie
-- cannot be forged for tokens its holder was never given.

ALTER TABLE user_appraisals
    ADD COLUMN IF NOT EXISTS is_anonymous BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE user_appraisals
    ADD COLUMN IF NOT EXISTS last_viewed_at TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX IF NOT EXISTS user_appraisals_anon_expiry_idx
    ON user_appraisals (last_viewed_at)
    WHERE is_anonymous;

CREATE TABLE IF NOT EXISTS appraisal_anonymous_owner (
    id      BIGINT PRIMARY KEY CHECK (id = 1),
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS appraisal_cookie_secrets (
    id         BIGINT PRIMARY KEY CHECK (id = 1),
    secret     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
