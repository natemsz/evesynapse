-- Schema step 110: per-item trading P&L on a pooled cost basis.
--
-- FIFO now matches inside a pool: by default every character of one
-- account ('acct', user id), or one character alone ('char') when
-- the player prefers per-character books, and each corporation on its
-- own ('corp'). Pool-level rows (fold checkpoints, open lots,
-- positions) are keyed by the pool's kind and id; realized sales stay
-- keyed by the character that sold, with the pool beside them.
--
-- Stored at the fold, read by the page:
--   * ledger_sale_tax: each sale's share of the sales tax paid in its
--     second, checked against the owner's prevailing rate;
--   * ledger_broker_alloc: broker fees tied to one order's type, when
--     exactly one of the owner's orders was placed or repriced within
--     a minute of the fee;
--   * trade_item_daily and trade_item_window: per seller, day (or
--     window) and type, the covered margin and every typed gap.
--
-- The v1 per-owner matching is retired: its rows are cleared here and
-- every pool refolds from the full stored ledger on its next pass.

CREATE TABLE IF NOT EXISTS user_profit_prefs (
    user_id          BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    pooled           BOOLEAN NOT NULL DEFAULT true,
    broker_in_margin BOOLEAN NOT NULL DEFAULT true,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per pool: who was in it when it last folded, so a change of
-- members (a character added, the per-character preference switched)
-- refolds the pool from scratch.
CREATE TABLE IF NOT EXISTS ledger_pool_state (
    pool_kind     TEXT NOT NULL,   -- 'acct' | 'char' | 'corp'
    pool_id       BIGINT NOT NULL,
    members       TEXT NOT NULL,   -- sorted member ids, comma separated
    folded_at     TIMESTAMPTZ NOT NULL,
    refolded_at   TIMESTAMPTZ NOT NULL,
    refold_reason TEXT NOT NULL,   -- 'new' | 'method' | 'members'
    PRIMARY KEY (pool_kind, pool_id)
);

-- Stretches a member's ledger cannot cover: two journal fetches more
-- than ESI's 30-day window apart.
CREATE TABLE IF NOT EXISTS ledger_holes (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    from_at    TIMESTAMPTZ NOT NULL,
    to_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, from_at)
);

-- Each placement or reprice of an owner's order the snapshots showed
-- (ESI keeps only the latest issued time per order).
CREATE TABLE IF NOT EXISTS ledger_order_issues (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    order_id   BIGINT NOT NULL,
    issued     TIMESTAMPTZ NOT NULL,
    type_id    BIGINT NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, order_id, issued)
);
CREATE INDEX IF NOT EXISTS ledger_order_issues_at_idx ON ledger_order_issues (owner_kind, owner_id, issued);

CREATE TABLE IF NOT EXISTS ledger_sale_tax (
    owner_kind     TEXT NOT NULL,
    owner_id       BIGINT NOT NULL,
    transaction_id BIGINT NOT NULL,
    at             TIMESTAMPTZ NOT NULL,
    tax            DOUBLE PRECISION NOT NULL,
    paired         BOOLEAN NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, transaction_id)
);
CREATE INDEX IF NOT EXISTS ledger_sale_tax_at_idx ON ledger_sale_tax (owner_kind, owner_id, at);

CREATE TABLE IF NOT EXISTS ledger_broker_alloc (
    owner_kind TEXT NOT NULL,
    owner_id   BIGINT NOT NULL,
    entry_id   BIGINT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    order_id   BIGINT NOT NULL,
    type_id    BIGINT NOT NULL,
    amount     DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (owner_kind, owner_id, entry_id)
);
CREATE INDEX IF NOT EXISTS ledger_broker_alloc_at_idx ON ledger_broker_alloc (owner_kind, owner_id, at);

-- A cost basis the player entered for stock no ledger row covers
-- (loot, contract items, gifts), as a lot in the pool's stream.
CREATE TABLE IF NOT EXISTS ledger_cost_basis_manual (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pool_kind  TEXT NOT NULL,
    pool_id    BIGINT NOT NULL,
    type_id    BIGINT NOT NULL,
    at         TIMESTAMPTZ NOT NULL,
    quantity   BIGINT NOT NULL,
    unit_cost  DOUBLE PRECISION NOT NULL,
    note       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS ledger_cost_basis_manual_pool_idx ON ledger_cost_basis_manual (pool_kind, pool_id, type_id);

ALTER TABLE trade_realized ADD COLUMN IF NOT EXISTS pool_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE trade_realized ADD COLUMN IF NOT EXISTS pool_id BIGINT NOT NULL DEFAULT 0;
-- Why the unmatched units have no cost: 'before_history', 'ledger_hole'
-- or 'no_buy' ('' when every unit matched).
ALTER TABLE trade_realized ADD COLUMN IF NOT EXISTS gap_kind TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS trade_realized_pool_idx ON trade_realized (pool_kind, pool_id, type_id);

-- Per seller, day and type. Covered = matched units of sales whose tax
-- paired; the gap columns count units by reason.
CREATE TABLE IF NOT EXISTS trade_item_daily (
    owner_kind     TEXT NOT NULL,
    owner_id       BIGINT NOT NULL,
    day            TEXT NOT NULL,
    type_id        BIGINT NOT NULL,
    units          BIGINT NOT NULL DEFAULT 0,
    revenue        DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_units      BIGINT NOT NULL DEFAULT 0,
    cov_revenue    DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_cost       DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_tax        DOUBLE PRECISION NOT NULL DEFAULT 0,
    broker         DOUBLE PRECISION NOT NULL DEFAULT 0,
    gap_before_history BIGINT NOT NULL DEFAULT 0,
    gap_ledger_hole    BIGINT NOT NULL DEFAULT 0,
    gap_no_buy         BIGINT NOT NULL DEFAULT 0,
    gap_no_tax         BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_id, day, type_id)
);

-- The same per seller and type over the last 7, 30, 90 and 365 days
-- as of a day, with the previous window of equal length beside it.
CREATE TABLE IF NOT EXISTS trade_item_window (
    owner_kind     TEXT NOT NULL,
    owner_id       BIGINT NOT NULL,
    win            INTEGER NOT NULL,
    type_id        BIGINT NOT NULL,
    as_of_day      TEXT NOT NULL,
    units          BIGINT NOT NULL DEFAULT 0,
    revenue        DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_units      BIGINT NOT NULL DEFAULT 0,
    cov_revenue    DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_cost       DOUBLE PRECISION NOT NULL DEFAULT 0,
    cov_tax        DOUBLE PRECISION NOT NULL DEFAULT 0,
    broker         DOUBLE PRECISION NOT NULL DEFAULT 0,
    gap_before_history BIGINT NOT NULL DEFAULT 0,
    gap_ledger_hole    BIGINT NOT NULL DEFAULT 0,
    gap_no_buy         BIGINT NOT NULL DEFAULT 0,
    gap_no_tax         BIGINT NOT NULL DEFAULT 0,
    prev_cov_net   DOUBLE PRECISION NOT NULL DEFAULT 0, -- revenue - cost - tax
    prev_broker    DOUBLE PRECISION NOT NULL DEFAULT 0,
    prev_units     BIGINT NOT NULL DEFAULT 0,
    prev_cov_units BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_id, win, type_id)
);

-- Retire v1's per-owner matching: every pool refolds from the stored
-- ledger on its next pass (no ledger_pool_state row yet).
TRUNCATE trade_realized, trade_open_lots, trade_fold_checkpoints, trade_positions;
