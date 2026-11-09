-- Schema step 136: durable ETag cache for ESI GETs.
--
-- The ESI client's general ETag cache (singleflight + ETag, 2026-11-09)
-- keeps one row per cached answer: the full path including its query
-- string, scoped by the access token that fetched it, so two
-- characters never share a cached answer. The token enters the key
-- only as a hash; a raw token is never written here. The in-memory
-- tier is the hot path; this table is what survives a restart, so a
-- restart does not re-download every resource. Rows expire by
-- ESI's own cache window (expires_at); an expired row is deleted
-- when it is next looked up, never served.
CREATE TABLE IF NOT EXISTS esi_etag_cache (
    cache_key  TEXT PRIMARY KEY,
    etag       TEXT NOT NULL,
    body       BYTEA NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
