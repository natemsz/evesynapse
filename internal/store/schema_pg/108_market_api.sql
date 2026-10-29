-- Schema step 108: the market API.
--
-- api_keys: a player's read-only market API keys. Only the key's
-- SHA-256 is stored, with its first characters to tell keys apart;
-- a key is shown once, when it is made. Revoking one stops it at
-- once. Each key carries its own daily request quota.
--
-- api_key_usage: requests per key per day, flushed from memory by
-- the worker, so a busy key costs no write per request.
--
-- abyssal_wants: mutated items an API caller asked to have priced
-- that the corpus has not seen. The worker reads their rolls from
-- ESI on its own budget and the scoring pass prices them; a caller
-- asks again later. Capped per key per day and in total.
--
-- blueprint_research_medians: the median ask of single-blueprint
-- contracts for a researched original, per type and material and
-- time efficiency, from the live public contract corpus, at the
-- same minimum sample the copy medians keep. A researched original
-- prices from its exact research level here; the market only sells
-- unresearched originals.

CREATE TABLE IF NOT EXISTS api_keys (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    daily_quota  BIGINT NOT NULL DEFAULT 20000,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_keys_user_idx ON api_keys (user_id);

CREATE TABLE IF NOT EXISTS api_key_usage (
    key_id   BIGINT NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    day      TEXT NOT NULL,
    requests BIGINT NOT NULL DEFAULT 0,
    refused  BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, day)
);

CREATE TABLE IF NOT EXISTS abyssal_wants (
    item_id      BIGINT PRIMARY KEY,
    type_id      BIGINT NOT NULL,
    key_id       BIGINT NOT NULL REFERENCES api_keys (id) ON DELETE CASCADE,
    requested_at TIMESTAMPTZ NOT NULL,
    state        TEXT NOT NULL DEFAULT 'pending',
    attempts     BIGINT NOT NULL DEFAULT 0,
    tried_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS abyssal_wants_pending_idx ON abyssal_wants (requested_at) WHERE state = 'pending';

CREATE TABLE IF NOT EXISTS blueprint_research_medians (
    type_id      BIGINT NOT NULL,
    me           BIGINT NOT NULL,
    te           BIGINT NOT NULL,
    median_price DOUBLE PRECISION NOT NULL,
    sample_count BIGINT NOT NULL,
    computed_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (type_id, me, te)
);
