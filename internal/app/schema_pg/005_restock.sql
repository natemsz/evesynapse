-- Schema step 005 (v0.3.34): restock planner targets.
--
-- restock_targets: per-user target stock levels for market items.
-- The restock planner page compares these against the user's open
-- sell orders to show what's listed, what's short, and the
-- estimated restock cost. min_margin_pct is stored for future
-- margin-threshold alerts (not yet enforced).

CREATE TABLE IF NOT EXISTS restock_targets (
    user_id       BIGINT NOT NULL,
    type_id       BIGINT NOT NULL,
    target_qty    BIGINT NOT NULL DEFAULT 0,
    min_margin_pct DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, type_id)
);
CREATE INDEX IF NOT EXISTS idx_restock_targets_user
    ON restock_targets (user_id);
