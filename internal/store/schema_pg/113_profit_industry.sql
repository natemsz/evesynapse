-- Schema step 113: per-blueprint industry P&L.
--
-- Three figures per blueprint over the same runs:
--   (a) realized: units actually sold (or marked lost), less what
--       they cost: the material lots FIFO consumed at install (any
--       material no lot covered at its install-day price, flagged),
--       the job cost ESI reports, and the sales' tax;
--   (b) at the time: inputs at their install-day price, output at
--       its delivery-day price, the job cost as paid;
--   (c) at today's prices: the same runs' recipe at today's stat
--       pack, in the region and on the side the player chose.
--
-- Materials a job consumed are not in ESI: they are derived from the
-- SDE recipe, the blueprint's ME and the facility's material bonus,
-- each stamped with where it came from. The ME is read when the job
-- is first seen delivered (a blueprint cannot be researched while it
-- runs a job); the bonus is the player's declared one, else none.

ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS blueprint_type_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS runs BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS me BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS me_source TEXT NOT NULL DEFAULT '';         -- 'at_delivery' | 'unknown'
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS material_bonus DOUBLE PRECISION NOT NULL DEFAULT 0; -- percent
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS bonus_source TEXT NOT NULL DEFAULT 'none_assumed'; -- | 'user_declared'
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS facility_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS installed_at TIMESTAMPTZ;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS job_cost DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS eiv DOUBLE PRECISION NOT NULL DEFAULT 0;     -- estimated item value when landed
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS price_region BIGINT NOT NULL DEFAULT 0;
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS inputs_at_time DOUBLE PRECISION NOT NULL DEFAULT 0;
-- 'install_day' when every input had a stat pack row that day, else
-- 'at_landing' (some priced at the house price when the job landed),
-- or 'unpriced'.
ALTER TABLE ledger_jobs ADD COLUMN IF NOT EXISTS inputs_source TEXT NOT NULL DEFAULT '';

-- What a job consumed, per material, and how much of it FIFO found in
-- the pool's stock (the rest is valued at unit_at_time).
CREATE TABLE IF NOT EXISTS ledger_job_materials (
    owner_kind   TEXT NOT NULL,
    owner_id     BIGINT NOT NULL,
    job_id       BIGINT NOT NULL,
    type_id      BIGINT NOT NULL,
    quantity     BIGINT NOT NULL,
    unit_at_time DOUBLE PRECISION NOT NULL DEFAULT 0,
    fifo_qty     BIGINT NOT NULL DEFAULT 0,
    fifo_cost    DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (owner_kind, owner_id, job_id, type_id)
);
CREATE INDEX IF NOT EXISTS ledger_job_materials_type_idx ON ledger_job_materials (owner_kind, type_id);

-- Open lots carry their source across a fold checkpoint, so a sale
-- after it still knows which job built what it sold.
ALTER TABLE trade_open_lots ADD COLUMN IF NOT EXISTS ref_id BIGINT NOT NULL DEFAULT 0;

-- What became of a job's output: units a sale took under FIFO (clean
-- when no bought stock of the type was open beside it, mixed when it
-- was), units the player linked, sold off-market or marked lost.
CREATE TABLE IF NOT EXISTS job_output_links (
    pool_kind TEXT NOT NULL,
    pool_id   BIGINT NOT NULL,
    job_id    BIGINT NOT NULL,
    type_id   BIGINT NOT NULL,
    link_kind TEXT NOT NULL,  -- 'fifo_clean' | 'fifo_mixed' | 'linked' | 'manual_sale' | 'lost'
    ref_id    BIGINT NOT NULL, -- transaction id, or the manual event id
    owner_id  BIGINT NOT NULL DEFAULT 0,
    at        TIMESTAMPTZ NOT NULL,
    units     BIGINT NOT NULL,
    revenue   DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (pool_kind, pool_id, job_id, link_kind, ref_id)
);
CREATE INDEX IF NOT EXISTS job_output_links_type_idx ON job_output_links (pool_kind, pool_id, type_id, at);

-- The player's own word on a job's output.
CREATE TABLE IF NOT EXISTS job_manual_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    pool_kind      TEXT NOT NULL,
    pool_id        BIGINT NOT NULL,
    job_id         BIGINT NOT NULL,
    kind           TEXT NOT NULL,  -- 'sale' | 'lost' | 'link'
    units          BIGINT NOT NULL DEFAULT 0,
    unit_price     DOUBLE PRECISION NOT NULL DEFAULT 0,
    transaction_id BIGINT NOT NULL DEFAULT 0,
    at             TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS job_manual_events_pool_idx ON job_manual_events (pool_kind, pool_id, job_id);

-- A recipe's value per run at today's stat pack, per region, ME and
-- material bonus (basis points), shared by every player. Rewritten
-- when the region's sweep completes, for the recipes some summary
-- asks for.
CREATE TABLE IF NOT EXISTS industry_recipe_values (
    region_id         BIGINT NOT NULL,
    blueprint_type_id BIGINT NOT NULL,
    me                BIGINT NOT NULL,
    bonus_bp          INTEGER NOT NULL,
    product_type_id   BIGINT NOT NULL,
    units_per_run     BIGINT NOT NULL,
    inputs_sell       DOUBLE PRECISION NOT NULL DEFAULT 0,
    inputs_buy        DOUBLE PRECISION NOT NULL DEFAULT 0,
    output_sell       DOUBLE PRECISION NOT NULL DEFAULT 0,
    output_buy        DOUBLE PRECISION NOT NULL DEFAULT 0,
    eiv               DOUBLE PRECISION NOT NULL DEFAULT 0,
    priced            BOOLEAN NOT NULL DEFAULT false,
    missing_type_id   BIGINT NOT NULL DEFAULT 0,
    fallback_used     BOOLEAN NOT NULL DEFAULT false,
    computed_at       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region_id, blueprint_type_id, me, bonus_bp)
);

-- Per pool and blueprint: the three figures and what covers them.
CREATE TABLE IF NOT EXISTS industry_bp_summary (
    pool_kind         TEXT NOT NULL,
    pool_id           BIGINT NOT NULL,
    blueprint_type_id BIGINT NOT NULL,
    product_type_id   BIGINT NOT NULL,
    region_id         BIGINT NOT NULL,
    side              TEXT NOT NULL,
    me                BIGINT NOT NULL,
    bonus_bp          INTEGER NOT NULL,
    jobs              BIGINT NOT NULL DEFAULT 0,
    runs              BIGINT NOT NULL DEFAULT 0,
    units             BIGINT NOT NULL DEFAULT 0,
    units_sold        BIGINT NOT NULL DEFAULT 0,
    units_lost        BIGINT NOT NULL DEFAULT 0,
    units_mixed       BIGINT NOT NULL DEFAULT 0,
    units_at_market   BIGINT NOT NULL DEFAULT 0, -- sold units whose job had materials no lot covered
    a_revenue         DOUBLE PRECISION NOT NULL DEFAULT 0,
    a_tax             DOUBLE PRECISION NOT NULL DEFAULT 0,
    a_cost            DOUBLE PRECISION NOT NULL DEFAULT 0,
    b_output          DOUBLE PRECISION NOT NULL DEFAULT 0,
    b_inputs          DOUBLE PRECISION NOT NULL DEFAULT 0,
    b_job             DOUBLE PRECISION NOT NULL DEFAULT 0,
    b_priced          BOOLEAN NOT NULL DEFAULT false,
    b_at_landing      BOOLEAN NOT NULL DEFAULT false,
    job_cost_per_run  DOUBLE PRECISION NOT NULL DEFAULT 0, -- the latest job's
    eiv_per_run       DOUBLE PRECISION NOT NULL DEFAULT 0, -- the latest job's
    tax_rate          DOUBLE PRECISION NOT NULL DEFAULT 0,
    broker_rate       DOUBLE PRECISION NOT NULL DEFAULT 0,
    c_total           DOUBLE PRECISION NOT NULL DEFAULT 0,
    c_priced          BOOLEAN NOT NULL DEFAULT false,
    c_missing_type_id BIGINT NOT NULL DEFAULT 0,
    c_fallback        BOOLEAN NOT NULL DEFAULT false,
    c_as_of           TIMESTAMPTZ,
    folded_at         TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (pool_kind, pool_id, blueprint_type_id)
);
CREATE INDEX IF NOT EXISTS industry_bp_summary_recipe_idx ON industry_bp_summary (region_id, blueprint_type_id, me, bonus_bp);

CREATE TABLE IF NOT EXISTS industry_facility_bonuses (
    user_id        BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    facility_id    BIGINT NOT NULL,
    material_bonus DOUBLE PRECISION NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, facility_id)
);

ALTER TABLE user_profit_prefs ADD COLUMN IF NOT EXISTS industry_region BIGINT NOT NULL DEFAULT 10000002;
ALTER TABLE user_profit_prefs ADD COLUMN IF NOT EXISTS industry_side TEXT NOT NULL DEFAULT 'sell';

-- Jobs landed before this step carry no recipe facts (blueprint_type_id
-- 0): the next pass fills them in for every job ESI's window still
-- shows; older ones keep counting as stock but stay out of the
-- per-blueprint figures. Every pool refolds so materials are consumed
-- from its stock.
UPDATE ledger_state SET jobs_hash = '';
UPDATE ledger_pool_state SET members = '';
