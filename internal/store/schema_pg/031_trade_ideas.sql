-- Schema step 030: stored trade ideas for the home page.
--
-- The worker asks Cloudflare's Gemma model about the Forge
-- spread-scanner rows every so often and stores the answer
-- here; the home widget renders the stored answer only. One
-- row, replaced on each refresh: the widget never waits on the
-- model, and a failed refresh keeps the previous answer (serve
-- the stale snapshot, like every other fetch path).

CREATE TABLE IF NOT EXISTS trade_ideas (
    id     BIGINT PRIMARY KEY CHECK (id = 1),
    body   TEXT NOT NULL DEFAULT '',
    as_of  timestamptz NOT NULL,
    model  TEXT NOT NULL DEFAULT ''
);
