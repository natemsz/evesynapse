-- Schema step 105: beaten orders.
--
-- order_lifecycle already tracks whether an open order is beaten at
-- its own station (beaten_now) and how often it has been
-- (outbid_events). The order-health pass now also writes, for sell
-- and buy orders alike, what beat it and what to do about it:
--
--   beaten_at       when the order last went from best to beaten
--   beating_price   the best competing price at its station (another
--                   player's: the owner's own characters never count)
--   beating_volume  how much stands at that price
--   suggested_price the next valid price past it (one tick under a
--                   sell, one tick over a buy)
--   unit_cost       the cost per unit the margin is measured against:
--                   for a sell, the open position's cost basis from
--                   the profit ledger; for a buy, the typical sell
--                   it could be resold at in its region
--   margin_pct      the margin at the suggested price after the
--                   owner's observed sales tax rate; NULL when no cost
--                   is on record
--   tax_rate        that observed rate (0 when none seen)
--   beaten_checked  when the pass last priced the order's book

ALTER TABLE order_lifecycle
    ADD COLUMN IF NOT EXISTS beaten_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS beating_price   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS beating_volume  BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS suggested_price DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS unit_cost       DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS margin_pct      DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS tax_rate        DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS beaten_checked  TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS order_lifecycle_beaten_idx
    ON order_lifecycle (character_id) WHERE beaten_now = 1 AND closed_at IS NULL;
