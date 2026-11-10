-- Schema step 137: live contract asks behind an unpriced type.
--
-- A type the true-value pass cannot price (too little evidence)
-- may still have live single-item public contracts asking for it.
-- The pass records their lowest and highest ask and how many there
-- are, so readers can show them beside the gap as unverified asks.
-- They are never a price: no total, sort or estimate reads them.
ALTER TABLE true_values
    ADD COLUMN IF NOT EXISTS ask_low   DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS ask_high  DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS ask_count BIGINT NOT NULL DEFAULT 0;
