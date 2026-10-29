-- Schema step 104: the profit ledger.
--
-- ESI keeps 30 days of wallet journal and transactions, and the
-- snapshots hold only the latest window. These tables keep every
-- row ever seen, per owner (a character, or a corporation from its
-- division journals), so profit can be told over any span.
--
-- From the ledger, the worker writes at ingest:
--   * trade_realized: each sell matched first-in first-out against
--     the owner's earlier buys and industry output of that type, its
--     cost basis, and any quantity no lot covered;
--   * trade_positions: the lots still open per type, capped by what
--     the owner still holds;
--   * profit_daily: income and costs per day and activity (trading,
--     industry, bounties and missions, planetary industry, contracts,
--     other sales, transfers, upkeep), each net of its own fees, and
--     for characters the day's unattributed remainder: the net worth
--     change no activity explains.
-- Pages read these rows only.

CREATE TABLE IF NOT EXISTS ledger_journal (
    owner_kind      TEXT NOT NULL,   -- 'char' | 'corp'
    owner_id        BIGINT NOT NULL,
    entry_id        BIGINT NOT NULL,
    at              TIMESTAMPTZ NOT NULL,
    ref_type        TEXT NOT NULL,
    activity        TEXT NOT NULL,
    amount          DOUBLE PRECISION NOT NULL,
    first_party_id  BIGINT NOT NULL DEFAULT 0,
    second_party_id BIGINT NOT NULL DEFAULT 0,
    context_id      BIGINT NOT NULL DEFAULT 0,
    context_type    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner_kind, owner_id, entry_id)
);
CREATE INDEX IF NOT EXISTS ledger_journal_day_idx ON ledger_journal (owner_kind, owner_id, at);

CREATE TABLE IF NOT EXISTS ledger_txns (
    owner_kind     TEXT NOT NULL,
    owner_id       BIGINT NOT NULL,
    transaction_id BIGINT NOT NULL,
    at             TIMESTAMPTZ NOT NULL,
    type_id        BIGINT NOT NULL,
    quantity       BIGINT NOT NULL,
    unit_price     DOUBLE PRECISION NOT NULL,
    is_buy         BOOLEAN NOT NULL,
    location_id    BIGINT NOT NULL DEFAULT 0,
    journal_ref_id BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_id, transaction_id)
);
CREATE INDEX IF NOT EXISTS ledger_txns_type_idx ON ledger_txns (owner_kind, owner_id, type_id, at);

-- Delivered manufacturing and reaction jobs, valued when first seen
-- delivered: output and materials at the house prices of that day,
-- so later price moves never rewrite what a job made.
CREATE TABLE IF NOT EXISTS ledger_jobs (
    owner_kind      TEXT NOT NULL,
    owner_id        BIGINT NOT NULL,
    job_id          BIGINT NOT NULL,
    at              TIMESTAMPTZ NOT NULL,
    activity_id     BIGINT NOT NULL,
    product_type_id BIGINT NOT NULL,
    units           BIGINT NOT NULL,
    output_value    DOUBLE PRECISION NOT NULL DEFAULT 0,
    materials_cost  DOUBLE PRECISION NOT NULL DEFAULT 0,
    priced          BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (owner_kind, owner_id, job_id)
);

CREATE INDEX IF NOT EXISTS ledger_jobs_type_idx ON ledger_jobs (owner_kind, owner_id, product_type_id);

-- The fold checkpoint per type: the day-aligned time the type's
-- realized rows are final up to, and the open lots at that moment.
-- A refold starts from the checkpoint instead of the owner's whole
-- history: new lots land inside the ESI window, which the lag in
-- profit_ledger.go clears by days, so nothing new can arrive behind
-- a checkpoint.
CREATE TABLE IF NOT EXISTS trade_fold_checkpoints (
    owner_kind    TEXT NOT NULL,
    owner_id      BIGINT NOT NULL,
    type_id       BIGINT NOT NULL,
    checkpoint_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, type_id)
);
CREATE TABLE IF NOT EXISTS trade_open_lots (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    type_id    BIGINT NOT NULL,
    seq        BIGINT NOT NULL,
    quantity   BIGINT NOT NULL,
    unit_price DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, type_id, seq)
);

CREATE TABLE IF NOT EXISTS trade_realized (
    owner_kind        TEXT NOT NULL,
    owner_id          BIGINT NOT NULL,
    transaction_id    BIGINT NOT NULL,
    day               TEXT NOT NULL,
    type_id           BIGINT NOT NULL,
    quantity          BIGINT NOT NULL,
    revenue           DOUBLE PRECISION NOT NULL,
    cost              DOUBLE PRECISION NOT NULL,
    -- How much of the sale no earlier buy or job covered (loot,
    -- planetary output, gifts), and its revenue.
    unmatched_qty     BIGINT NOT NULL DEFAULT 0,
    unmatched_revenue DOUBLE PRECISION NOT NULL DEFAULT 0,
    planetary         BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (owner_kind, owner_id, transaction_id)
);
CREATE INDEX IF NOT EXISTS trade_realized_day_idx ON trade_realized (owner_kind, owner_id, day);

CREATE TABLE IF NOT EXISTS trade_positions (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    type_id    BIGINT NOT NULL,
    quantity   BIGINT NOT NULL,
    cost       DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, type_id)
);

CREATE TABLE IF NOT EXISTS profit_daily (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    day        TEXT NOT NULL,
    activity   TEXT NOT NULL,
    income     DOUBLE PRECISION NOT NULL DEFAULT 0,
    costs      DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_id, day, activity)
);

-- How far each owner's ledger has been folded into the tables above.
CREATE TABLE IF NOT EXISTS ledger_state (
    owner_kind  TEXT NOT NULL,
    owner_id    BIGINT NOT NULL,
    journal_at  TIMESTAMPTZ,
    txns_at     TIMESTAMPTZ,
    jobs_at     TIMESTAMPTZ,
    journal_hash TEXT NOT NULL DEFAULT '',
    txns_hash    TEXT NOT NULL DEFAULT '',
    jobs_hash    TEXT NOT NULL DEFAULT '',
    folded_at   TIMESTAMPTZ,
    PRIMARY KEY (owner_kind, owner_id)
);
