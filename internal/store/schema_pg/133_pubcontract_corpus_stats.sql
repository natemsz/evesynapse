-- Public contract corpus statistics (schema 133): the live
-- corpus size the contracts search page shows, counted by the
-- sweep when a region cycle completes and stored here, so a
-- render reads one row instead of counting the corpus. The
-- partial index keeps even that worker count index-only, and
-- the trigram index lets title searches ride an index instead
-- of scanning the corpus (pg_trgm is already enabled, schema
-- 059).

CREATE TABLE IF NOT EXISTS pub_contract_corpus_stats (
    id             INTEGER PRIMARY KEY,
    open_contracts BIGINT NOT NULL DEFAULT 0,
    computed_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS pub_contracts_live_idx
    ON pub_contracts (contract_id) WHERE departed_at IS NULL;

CREATE INDEX IF NOT EXISTS pub_contracts_title_trgm_idx
    ON pub_contracts USING gin (title gin_trgm_ops);
