-- Schema step 060: the public-contract item drain's failure
-- bookkeeping. Until now a contract whose item list failed to
-- fetch or decode (one known listing's payload does not parse)
-- sat in 'pending' forever and was retried on every pass,
-- spending lane budget on an answer that never changes.
--
-- items_attempts counts the drain's consecutive failures on a
-- row (decode and store failures included, not just fetch
-- errors); items_error keeps the latest failure's reason. At
-- the drain's threshold the row's items_state becomes
-- 'parked' -- terminal, like 'private' and 'gone', named for
-- what it is: set aside, on record, never retried. Nothing
-- reads these columns to price or search; they are the drain's
-- own ledger.

ALTER TABLE pub_contracts
    ADD COLUMN IF NOT EXISTS items_attempts BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pub_contracts
    ADD COLUMN IF NOT EXISTS items_error TEXT NOT NULL DEFAULT '';

-- The drain now walks newest first (fresh contracts get their
-- contents and appraisal within a cycle or two; the cold
-- backlog backfills behind them). Step 059's partial index is
-- keyed for the old oldest-first walk; this one serves the new
-- order over the same pending slice. The old index stays:
-- dropping objects is not this step's business.
CREATE INDEX IF NOT EXISTS pub_contracts_pending_newest_idx
    ON pub_contracts (date_issued DESC, contract_id DESC)
    WHERE items_state = 'pending';
