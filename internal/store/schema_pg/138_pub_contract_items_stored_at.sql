-- Schema step 138: when a public contract's item list arrived.
--
-- The true-value pass keeps the corpus contracts it mined per type
-- and mines a type again when a contract carrying it arrives: its
-- item list is stored, stamped here so the pass can range over the
-- new arrivals. Rows stored before this step keep a null stamp: the
-- pass mines every type afresh when it starts.
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS items_stored_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS pub_contracts_items_stored_idx
    ON pub_contracts (items_stored_at) WHERE items_stored_at IS NOT NULL;
