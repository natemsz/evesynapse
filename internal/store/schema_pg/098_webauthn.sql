-- Two-factor authentication (098): WebAuthn credentials, one-time
-- recovery codes, and the per-account second-factor audit trail.
-- A key enrolled on one hostname never asserts on another, so no
-- RP ID is stored: the RP ID and origins derive from the request
-- host on every ceremony (auth_webauthn.go).
CREATE TABLE IF NOT EXISTS webauthn_credentials (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id BYTEA NOT NULL UNIQUE,
    public_key BYTEA NOT NULL,
    sign_count BIGINT NOT NULL DEFAULT 0,
    transports TEXT NOT NULL DEFAULT '',
    attestation_format TEXT NOT NULL DEFAULT '',
    name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS webauthn_credentials_user_idx ON webauthn_credentials (user_id);

-- Recovery codes are shown once at enrollment and stored as hashes.
CREATE TABLE IF NOT EXISTS webauthn_recovery_codes (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash TEXT NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webauthn_recovery_codes_user_idx ON webauthn_recovery_codes (user_id);

-- Second-factor events: enroll, assert_ok, assert_fail, recover,
-- remove, codes_regenerated. Pruned past a year by the worker.
CREATE TABLE IF NOT EXISTS webauthn_events (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS webauthn_events_user_idx ON webauthn_events (user_id, created_at DESC);
