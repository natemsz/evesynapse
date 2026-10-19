-- Schema step 065: remembered appraisal pricing basis.
--
-- The last pricing basis (hub region, mode, percent) a
-- signed-in user appraised under, so the form opens on it.
-- Anonymous visitors remember theirs in a signed cookie
-- instead; nothing is stored for them here.

CREATE TABLE IF NOT EXISTS user_appraisal_prefs (
    user_id    BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    hub_region BIGINT NOT NULL DEFAULT 10000002,
    mode       TEXT NOT NULL DEFAULT 'sell',
    percent    DOUBLE PRECISION NOT NULL DEFAULT 100,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
