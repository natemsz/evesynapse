-- Schema step 183: doctrine readiness, folded at ingest.
--
-- For every account, which of its pilots can fly each fit of the
-- doctrines its corporations, alliances and communities keep, and how
-- close the nearest pilot is when one cannot. "Can fly" is the fitting
-- page's own test: every skill the hull, its modules, charges and
-- implants ask for, prerequisites included, trained to the level
-- asked. The worker folds these rows from the stored skill snapshots
-- (doctrine_readiness.go) whenever an account's pilots, their skills,
-- its doctrine owners or the doctrines' fits change; the home's
-- Doctrine Readiness widget only reads them. An account with no fold
-- row yet is folded on its next worker pass, so existing accounts
-- need no separate backfill.
CREATE TABLE IF NOT EXISTS doctrine_fit_readiness (
    user_id        BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    fit_id         BIGINT NOT NULL REFERENCES doctrine_fits (id) ON DELETE CASCADE,
    doctrine_id    BIGINT NOT NULL,
    ready_ids      BIGINT[] NOT NULL DEFAULT '{}',
    -- near_*: the pilot who cannot fly the fit yet with the fewest
    -- skill points left to train for it; 0 when every pilot can.
    near_character BIGINT NOT NULL DEFAULT 0,
    near_missing   TEXT NOT NULL DEFAULT '',
    near_missing_n INTEGER NOT NULL DEFAULT 0,
    near_sp        BIGINT NOT NULL DEFAULT 0,
    computed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, fit_id)
);
CREATE INDEX IF NOT EXISTS doctrine_fit_readiness_doctrine_idx ON doctrine_fit_readiness (user_id, doctrine_id);

-- One row per account and doctrine: how many fits and pilots, how
-- many of each are ready, and the nearest gap across its fits for a
-- pilot who can fly none of them.
CREATE TABLE IF NOT EXISTS doctrine_readiness (
    user_id        BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    doctrine_id    BIGINT NOT NULL REFERENCES doctrines (id) ON DELETE CASCADE,
    fits           INTEGER NOT NULL,
    fits_flyable   INTEGER NOT NULL,
    pilots         INTEGER NOT NULL,
    pilots_ready   INTEGER NOT NULL,
    near_character BIGINT NOT NULL DEFAULT 0,
    near_fit       BIGINT NOT NULL DEFAULT 0,
    near_missing   TEXT NOT NULL DEFAULT '',
    near_missing_n INTEGER NOT NULL DEFAULT 0,
    near_sp        BIGINT NOT NULL DEFAULT 0,
    computed_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, doctrine_id)
);

-- The fold's watermark per account: what it was computed from.
CREATE TABLE IF NOT EXISTS doctrine_readiness_folds (
    user_id     BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    inputs      TEXT NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL
);
