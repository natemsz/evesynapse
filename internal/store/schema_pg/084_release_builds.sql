-- Builds the release learner has recorded (schema 084): the
-- SHA-256 of every binary asset on the owner's published
-- GitHub releases, fetched by the swarm module's learner
-- (internal/app/swarm/releases.go) and kept for good -- a hash
-- learned here stays known after its release scrolls off the
-- API's first page. Together with the seeded library in code,
-- this table is how the Admin page's Build column tells a
-- mainline build from a fork. Display data only: the trust
-- model never reads it.
CREATE TABLE IF NOT EXISTS release_builds (
    hash       TEXT PRIMARY KEY,
    kind       TEXT NOT NULL,
    label      TEXT NOT NULL,
    learned_at TIMESTAMPTZ NOT NULL
);
