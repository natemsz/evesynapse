-- Scans (schema 077): pasted d-scans, local lists, fleet
-- compositions and probe results (internal/scan reads them,
-- internal/app/scans.go enriches and stores them).
--
-- A scan is unlisted: reachable by its random id, never listed for
-- anyone but who pasted it (user_id when signed in; the session
-- remembers an anonymous paster's own). result is the enriched
-- reading as of the paste, JSON -- a local list's affiliations are
-- what they were then. Scans are kept 90 days.
CREATE TABLE IF NOT EXISTS scans (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    system_id   BIGINT NOT NULL DEFAULT 0,
    user_id     BIGINT NOT NULL DEFAULT 0,
    raw         TEXT NOT NULL,
    result      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS scans_created_idx ON scans (created_at);
CREATE INDEX IF NOT EXISTS scans_user_idx ON scans (user_id, created_at DESC) WHERE user_id > 0;
