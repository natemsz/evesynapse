-- Migration 019 (Next-1): daily wallet-balance history. The
-- worker writes at most one row per character per day from the
-- wallet snapshot it already keeps, so balance (and net-worth,
-- when the price cache makes it computable) history accrues
-- from the day this ships. Graphs read this table later; today
-- it only records. net_worth stays NULL on days the estimate
-- couldn't be computed, so "unknown" never reads as zero.
CREATE TABLE IF NOT EXISTS wallet_history (
    user_id      INTEGER NOT NULL,
    character_id INTEGER NOT NULL,
    day          TEXT    NOT NULL, -- UTC calendar day, YYYY-MM-DD
    balance      REAL    NOT NULL, -- wallet balance at last fetch that day
    net_worth    REAL,             -- wallet + priced assets + buy escrow, when computable
    sampled_at   TEXT    NOT NULL, -- RFC3339, when the row was last written
    PRIMARY KEY (user_id, character_id, day)
);
