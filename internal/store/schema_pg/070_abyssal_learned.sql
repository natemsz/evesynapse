-- Abyssal learned correction (schema 070).
--
-- Confirmed sales. A public contract that leaves the market
-- before its expiry is asked about once on its own items route:
-- ESI answers 204 for a contract accepted (or expired, which the
-- expiry date rules out) and 404 for one deleted or long gone.
-- A 204 stamps the listing's sightings sold_at, and the corpus
-- reads that price as a sale, not an ask. Anything else stamps
-- only sale_probed_at: no answer is ever read as "not sold".
-- A finished contract in a signed-in character's own contract
-- list stamps sold_at the same way. Aggregate evidence only,
-- never shown per contract or per user.
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS sold_at TIMESTAMPTZ;
ALTER TABLE abyssal_sightings ADD COLUMN IF NOT EXISTS sale_probed_at TIMESTAMPTZ;
-- The true-value crawl's watched listings get the same answer:
-- a listing confirmed accepted speaks in the recorded-sale tier,
-- not the likely-sale one.
ALTER TABLE true_value_contract_observations ADD COLUMN IF NOT EXISTS sold BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS abyssal_sightings_sale_probe_idx
    ON abyssal_sightings (departed_at)
    WHERE depart_kind = 'vanished' AND sale_probed_at IS NULL;

-- One row per family: the learned correction's out-of-sample
-- record. The worker fits a monotone boosted correction to the
-- ladder on older listings and scores it on newer ones against
-- the plain ladder-times-factor; the correction is applied only
-- where it beat that on listings it never saw.
--   comps         clean comps the fit could use
--   n_eff         their effective count after weighting
--   holdout_n     newer comps the backtest scored
--   sales_n       of the comps, confirmed sales
--   mae_ladder    median absolute log error, ladder times factor
--   mae_model     median absolute log error, learned correction
--   bias_model    median signed log error, learned correction
--   use_model     whether the correction is applied
CREATE TABLE IF NOT EXISTS abyssal_family_fit (
    family     TEXT PRIMARY KEY,
    comps      BIGINT NOT NULL DEFAULT 0,
    n_eff      DOUBLE PRECISION NOT NULL DEFAULT 0,
    holdout_n  BIGINT NOT NULL DEFAULT 0,
    sales_n    BIGINT NOT NULL DEFAULT 0,
    mae_ladder DOUBLE PRECISION,
    mae_model  DOUBLE PRECISION,
    bias_model DOUBLE PRECISION,
    use_model  BOOLEAN NOT NULL DEFAULT FALSE,
    fitted_at  TIMESTAMPTZ NOT NULL
);

-- Which market adjustment an item's estimate carries: 'factor'
-- (the bounded per-family factor) or 'learned' (the correction).
-- market_calib keeps the multiplier applied either way.
ALTER TABLE abyssal_items ADD COLUMN IF NOT EXISTS market_basis TEXT NOT NULL DEFAULT 'factor';
