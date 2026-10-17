-- Schema step 056: public contract appraisal results.
--
-- The appraisal core (core's appraisal.go) prices every
-- stored contract line against the house price order -- our
-- own swept Forge typicals, then the stored true-value
-- estimated sell, then the labeled Fuzzwork fallback, else
-- honestly unpriced -- at ingest time, never at render. The
-- per-line result (unit price, line value, which tier spoke,
-- and when that tier's figure was current) lands on the item
-- row; the contract row carries the totals the search sorts
-- and flags by: offered and wanted value, the priced-line
-- coverage of the offered side, the tier mix the number is
-- made of, and the deal ratio (asking price over appraised
-- offered value) -- stored only when the offered side is
-- fully priced, because a ratio over a partial appraisal
-- would be a lie of precision.

ALTER TABLE pub_contract_items ADD COLUMN IF NOT EXISTS unit_price DOUBLE PRECISION;
ALTER TABLE pub_contract_items ADD COLUMN IF NOT EXISTS line_value DOUBLE PRECISION;
ALTER TABLE pub_contract_items ADD COLUMN IF NOT EXISTS price_tier TEXT NOT NULL DEFAULT '';
ALTER TABLE pub_contract_items ADD COLUMN IF NOT EXISTS price_asof TIMESTAMPTZ;

ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS offered_value DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS want_value DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS deal_ratio DOUBLE PRECISION;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS priced_lines BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS total_lines BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS tier_mix TEXT NOT NULL DEFAULT '';
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS priced_at TIMESTAMPTZ;

-- The deal sort and the deals-only filter ride this: only
-- fully covered exchanges and auctions carry a ratio at all.
CREATE INDEX IF NOT EXISTS pub_contracts_deal_idx
    ON pub_contracts (deal_ratio) WHERE deal_ratio IS NOT NULL;

-- The appraisal pass walks what is due: stored item lists not
-- yet priced, or priced before the pricing inputs last moved.
CREATE INDEX IF NOT EXISTS pub_contracts_appraisal_due_idx
    ON pub_contracts (priced_at) WHERE items_state = 'stored';
