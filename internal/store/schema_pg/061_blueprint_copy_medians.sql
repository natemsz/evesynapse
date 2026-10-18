-- Schema step 061: blueprint copy medians.
--
-- What the appraisal core may price a blueprint COPY from:
-- the median asking price of single-copy contracts in our
-- own public corpus, per blueprint type and remaining runs,
-- computed by the worker's copy-median pass (appraisal.go)
-- and read as a stored figure ever after. A copy is not its
-- original: originals trade on the market and copies never
-- do, so every type-level price for a blueprint type is the
-- ORIGINAL's price, and pricing a copy from it values the
-- wrong object. Rows exist only where the corpus holds at
-- least the pass's minimum sample of single-copy listings;
-- a copy with no row here is honestly unpriced.
--
-- sample_count rides along so readers can show what the
-- median stands on; computed_at is the figure's as-of.

CREATE TABLE IF NOT EXISTS blueprint_copy_medians (
    type_id      BIGINT NOT NULL,
    runs         BIGINT NOT NULL,
    median_price DOUBLE PRECISION NOT NULL,
    sample_count BIGINT NOT NULL,
    computed_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (type_id, runs)
);
