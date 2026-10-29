-- Per-blueprint industry P&L (schema 111): recipe facts on landed
-- jobs, the materials they consumed, what became of their output,
-- shared recipe values at today's prices, and the summary.

-- name: UpsertLedgerJob :exec
-- A job lands once with its recipe facts; a job landed before those
-- were kept (blueprint_type_id 0) takes them on its next sighting.
INSERT INTO ledger_jobs (owner_kind, owner_id, job_id, at, activity_id, product_type_id, units, output_value, materials_cost, priced,
                         blueprint_type_id, runs, me, me_source, material_bonus, bonus_source, facility_id, installed_at,
                         job_cost, eiv, price_region, inputs_at_time, inputs_source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
ON CONFLICT (owner_kind, owner_id, job_id) DO UPDATE SET
    blueprint_type_id = excluded.blueprint_type_id, runs = excluded.runs, me = excluded.me, me_source = excluded.me_source,
    material_bonus = excluded.material_bonus, bonus_source = excluded.bonus_source, facility_id = excluded.facility_id,
    installed_at = excluded.installed_at, job_cost = excluded.job_cost, eiv = excluded.eiv, price_region = excluded.price_region,
    inputs_at_time = excluded.inputs_at_time, inputs_source = excluded.inputs_source
WHERE ledger_jobs.blueprint_type_id = 0;

-- name: UpsertJobMaterialsBatch :exec
INSERT INTO ledger_job_materials (owner_kind, owner_id, job_id, type_id, quantity, unit_at_time)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, sqlc.arg(job_id)::bigint,
       unnest(sqlc.arg(type_ids)::bigint[]), unnest(sqlc.arg(quantities)::bigint[]), unnest(sqlc.arg(units_at_time)::float8[])
ON CONFLICT (owner_kind, owner_id, job_id, type_id) DO UPDATE SET
    quantity = excluded.quantity, unit_at_time = excluded.unit_at_time;

-- name: ListAdjustedPrices :many
-- CCP's adjusted prices: read only for the estimated item value the
-- game's own job cost formula uses, never to value an item.
SELECT type_id, adjusted_price FROM guide_prices WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);

-- name: ListRegionStatsDailyForTypes :many
SELECT type_id, typical_sell, typical_buy FROM market_region_stats_daily
WHERE region_id = sqlc.arg(region_id) AND day = sqlc.arg(day)::text AND type_id = ANY(sqlc.arg(type_ids)::bigint[]);

-- name: UpdateJobMaterialFIFO :exec
UPDATE ledger_job_materials SET fifo_qty = $5, fifo_cost = $6
WHERE owner_kind = $1 AND owner_id = $2 AND job_id = $3 AND type_id = $4;

-- name: DeleteJobOutputLinksFrom :exec
DELETE FROM job_output_links
WHERE pool_kind = $1 AND pool_id = $2 AND type_id = $3 AND at >= sqlc.arg(from_at)::timestamptz;

-- name: DeleteJobOutputLinksForPool :exec
DELETE FROM job_output_links WHERE pool_kind = $1 AND pool_id = $2;

-- name: InsertJobOutputLinksBatch :exec
INSERT INTO job_output_links (pool_kind, pool_id, job_id, type_id, link_kind, ref_id, owner_id, at, units, revenue)
SELECT sqlc.arg(pool_kind)::text, sqlc.arg(pool_id)::bigint, unnest(sqlc.arg(job_ids)::bigint[]), sqlc.arg(type_id)::bigint,
       unnest(sqlc.arg(link_kinds)::text[]), unnest(sqlc.arg(ref_ids)::bigint[]), unnest(sqlc.arg(owner_ids)::bigint[]),
       unnest(sqlc.arg(ats)::text[])::timestamptz, unnest(sqlc.arg(units)::bigint[]), unnest(sqlc.arg(revenues)::float8[])
ON CONFLICT (pool_kind, pool_id, job_id, link_kind, ref_id) DO UPDATE SET
    units = job_output_links.units + excluded.units, revenue = job_output_links.revenue + excluded.revenue;

-- name: GetPoolTradeRates :one
-- A pool's observed sales tax and broker fees against what it sold
-- over a span: the rates today's-price figures charge on output.
SELECT COALESCE((SELECT SUM(-j.amount) FROM ledger_journal j
                 WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
                   AND j.ref_type = 'transaction_tax' AND j.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS tax,
       COALESCE((SELECT SUM(-j.amount) FROM ledger_journal j
                 WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
                   AND j.ref_type = 'brokers_fee' AND j.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS broker,
       COALESCE((SELECT SUM(t.quantity * t.unit_price) FROM ledger_txns t
                 WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
                   AND NOT t.is_buy AND t.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS revenue;

-- name: DeleteIndustrySummaryForPool :exec
DELETE FROM industry_bp_summary WHERE pool_kind = $1 AND pool_id = $2;

-- name: FoldIndustrySummary :exec
-- Per blueprint across a pool's jobs that carry recipe facts:
--   (a) from the output links: revenue, the sales' tax share, and each
--       linked unit's share of its job's cost (materials as FIFO
--       consumed them, the rest at their install-day price, plus the
--       job cost);
--   (b) from the jobs: output at delivery, inputs at install, job cost.
-- The latest job sets the recipe (ME, bonus) today's figure reruns.
WITH jobs AS (
    SELECT j.*,
           COALESCE((SELECT SUM(m.fifo_cost + GREATEST(m.quantity - m.fifo_qty, 0) * m.unit_at_time)
                     FROM ledger_job_materials m
                     WHERE m.owner_kind = j.owner_kind AND m.owner_id = j.owner_id AND m.job_id = j.job_id), 0) AS mat_actual,
           EXISTS (SELECT 1 FROM ledger_job_materials m
                   WHERE m.owner_kind = j.owner_kind AND m.owner_id = j.owner_id AND m.job_id = j.job_id
                     AND m.fifo_qty < m.quantity) AS mat_at_market
    FROM ledger_jobs j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND j.blueprint_type_id > 0
), links AS (
    SELECT l.job_id, l.link_kind, l.units, l.revenue,
           COALESCE(s.tax * l.units / NULLIF(t.quantity, 0), 0) AS tax
    FROM job_output_links l
    LEFT JOIN ledger_txns t ON l.link_kind IN ('fifo_clean', 'fifo_mixed', 'linked')
         AND t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = l.owner_id AND t.transaction_id = l.ref_id
    LEFT JOIN ledger_sale_tax s ON s.owner_kind = t.owner_kind AND s.owner_id = t.owner_id AND s.transaction_id = t.transaction_id
    WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id)
), per_job AS (
    SELECT j.job_id, j.blueprint_type_id,
           SUM(CASE WHEN l.link_kind <> 'lost' THEN l.units ELSE 0 END) AS sold,
           SUM(CASE WHEN l.link_kind = 'lost' THEN l.units ELSE 0 END) AS lost,
           SUM(CASE WHEN l.link_kind = 'fifo_mixed' THEN l.units ELSE 0 END) AS mixed,
           SUM(CASE WHEN j.mat_at_market THEN l.units ELSE 0 END) AS at_market,
           SUM(l.revenue) AS revenue, SUM(l.tax) AS tax,
           SUM(l.units * (j.mat_actual + j.job_cost) / NULLIF(j.units, 0)) AS cost
    FROM jobs j JOIN links l ON l.job_id = j.job_id
    GROUP BY j.job_id, j.blueprint_type_id
), latest AS (
    SELECT DISTINCT ON (blueprint_type_id) blueprint_type_id, product_type_id, me, material_bonus,
           job_cost / NULLIF(runs, 0) AS job_cost_per_run, eiv / NULLIF(runs, 0) AS eiv_per_run
    FROM jobs ORDER BY blueprint_type_id, at DESC
)
INSERT INTO industry_bp_summary (pool_kind, pool_id, blueprint_type_id, product_type_id, region_id, side, me, bonus_bp,
    jobs, runs, units, units_sold, units_lost, units_mixed, units_at_market, a_revenue, a_tax, a_cost,
    b_output, b_inputs, b_job, b_priced, b_at_landing, job_cost_per_run, eiv_per_run, tax_rate, broker_rate, folded_at)
SELECT sqlc.arg(pool_kind)::text, sqlc.arg(pool_id)::bigint, j.blueprint_type_id, MAX(lt.product_type_id),
       sqlc.arg(region_id)::bigint, sqlc.arg(side)::text, MAX(lt.me), MAX(round(lt.material_bonus * 100))::int,
       COUNT(*), SUM(j.runs), SUM(j.units),
       COALESCE(MAX(p.sold), 0), COALESCE(MAX(p.lost), 0), COALESCE(MAX(p.mixed), 0), COALESCE(MAX(p.at_market), 0),
       COALESCE(MAX(p.revenue), 0), COALESCE(MAX(p.tax), 0), COALESCE(MAX(p.cost), 0),
       SUM(j.output_value), SUM(j.inputs_at_time), SUM(j.job_cost),
       bool_and(j.priced AND j.inputs_source <> 'unpriced'), bool_or(j.inputs_source = 'at_landing'),
       COALESCE(MAX(lt.job_cost_per_run), 0), COALESCE(MAX(lt.eiv_per_run), 0),
       sqlc.arg(tax_rate)::float8, sqlc.arg(broker_rate)::float8, sqlc.arg(folded_at)::timestamptz
FROM jobs j
JOIN latest lt ON lt.blueprint_type_id = j.blueprint_type_id
LEFT JOIN (
    SELECT blueprint_type_id, SUM(sold) AS sold, SUM(lost) AS lost, SUM(mixed) AS mixed, SUM(at_market) AS at_market,
           SUM(revenue) AS revenue, SUM(tax) AS tax, SUM(cost) AS cost
    FROM per_job GROUP BY blueprint_type_id
) p ON p.blueprint_type_id = j.blueprint_type_id
GROUP BY j.blueprint_type_id;

-- name: ListIndustryRecipeKeys :many
SELECT DISTINCT blueprint_type_id, me, bonus_bp FROM industry_bp_summary WHERE region_id = $1;

-- name: ListMissingRecipeKeysForPool :many
-- The recipes a pool's summary asks for that no stored value covers.
SELECT DISTINCT s.region_id, s.blueprint_type_id, s.me, s.bonus_bp
FROM industry_bp_summary s
WHERE s.pool_kind = $1 AND s.pool_id = $2
  AND NOT EXISTS (SELECT 1 FROM industry_recipe_values r
                  WHERE r.region_id = s.region_id AND r.blueprint_type_id = s.blueprint_type_id
                    AND r.me = s.me AND r.bonus_bp = s.bonus_bp);

-- name: UpsertIndustryRecipeValue :exec
INSERT INTO industry_recipe_values (region_id, blueprint_type_id, me, bonus_bp, product_type_id, units_per_run, inputs_sell,
    inputs_buy, output_sell, output_buy, eiv, priced, missing_type_id, fallback_used, computed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (region_id, blueprint_type_id, me, bonus_bp) DO UPDATE SET
    product_type_id = excluded.product_type_id, units_per_run = excluded.units_per_run,
    inputs_sell = excluded.inputs_sell, inputs_buy = excluded.inputs_buy, output_sell = excluded.output_sell,
    output_buy = excluded.output_buy, eiv = excluded.eiv, priced = excluded.priced,
    missing_type_id = excluded.missing_type_id, fallback_used = excluded.fallback_used, computed_at = excluded.computed_at;

-- name: ApplyIndustryRecipeValues :exec
-- Today's figure for the summaries a region's recipes serve (or one
-- pool's): the same runs at today's prices on the player's side, less
-- tax (and broker fees when selling through orders) at the pool's
-- observed rates, less the job cost scaled by how the inputs' value
-- moved since the latest job.
UPDATE industry_bp_summary s SET
    c_total = s.runs * (
        CASE WHEN s.side = 'buy' THEN r.output_buy * (1 - s.tax_rate) - r.inputs_buy
             ELSE r.output_sell * (1 - s.tax_rate - s.broker_rate) - r.inputs_sell END
        - CASE WHEN s.eiv_per_run > 0 THEN s.job_cost_per_run * r.eiv / s.eiv_per_run ELSE s.job_cost_per_run END),
    c_priced = r.priced, c_missing_type_id = r.missing_type_id, c_fallback = r.fallback_used, c_as_of = r.computed_at
FROM industry_recipe_values r
WHERE r.region_id = s.region_id AND r.blueprint_type_id = s.blueprint_type_id AND r.me = s.me AND r.bonus_bp = s.bonus_bp
  AND (sqlc.arg(region_id)::bigint = 0 OR s.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(pool_kind)::text = '' OR (s.pool_kind = sqlc.arg(pool_kind)::text AND s.pool_id = sqlc.arg(pool_id)::bigint));

-- name: ListIndustrySummary :many
-- The Industry section: blueprints for a set of pools, best realized
-- first.
SELECT * FROM industry_bp_summary
WHERE pool_kind = sqlc.arg(pool_kind) AND pool_id = ANY(sqlc.arg(pool_ids)::bigint[])
ORDER BY a_revenue - a_tax - a_cost DESC, b_output - b_inputs - b_job DESC, blueprint_type_id
LIMIT sqlc.arg(row_cap)::bigint;

-- name: ListPoolJobsForBlueprint :many
-- A blueprint's jobs in a pool, newest first, with what became of
-- each one's output.
SELECT j.owner_id, j.job_id, j.at, j.installed_at, j.runs, j.units, j.me, j.me_source, j.material_bonus, j.bonus_source,
       j.facility_id, j.job_cost, j.output_value, j.inputs_at_time, j.inputs_source, j.priced,
       COALESCE((SELECT SUM(l.units) FROM job_output_links l
                 WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id) AND l.job_id = j.job_id AND l.link_kind <> 'lost'), 0)::bigint AS sold,
       COALESCE((SELECT SUM(l.units) FROM job_output_links l
                 WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id) AND l.job_id = j.job_id AND l.link_kind = 'lost'), 0)::bigint AS lost,
       COALESCE((SELECT SUM(l.units) FROM job_output_links l
                 WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id) AND l.job_id = j.job_id AND l.link_kind = 'fifo_mixed'), 0)::bigint AS mixed,
       COALESCE((SELECT SUM(l.revenue) FROM job_output_links l
                 WHERE l.pool_kind = sqlc.arg(pool_kind) AND l.pool_id = sqlc.arg(pool_id) AND l.job_id = j.job_id), 0)::float8 AS revenue
FROM ledger_jobs j
WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND j.blueprint_type_id = sqlc.arg(blueprint_type_id)
ORDER BY j.at DESC
LIMIT 50;

-- name: ListRecentSellsOfType :many
-- A pool's recent market sales of a type, for linking one to a job.
SELECT t.owner_id, t.transaction_id, t.at, t.quantity, t.unit_price
FROM ledger_txns t
WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND t.type_id = sqlc.arg(type_id) AND NOT t.is_buy
ORDER BY t.at DESC
LIMIT 20;

-- name: GetPoolJob :one
SELECT owner_id, job_id, at, product_type_id, units FROM ledger_jobs
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND job_id = sqlc.arg(job_id);

-- name: GetPoolSell :one
SELECT owner_id, transaction_id, at, type_id, quantity FROM ledger_txns
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND transaction_id = sqlc.arg(transaction_id) AND NOT is_buy;

-- name: InsertJobManualEvent :exec
INSERT INTO job_manual_events (pool_kind, pool_id, job_id, kind, units, unit_price, transaction_id, at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListUserFacilityBonuses :many
SELECT facility_id, material_bonus FROM industry_facility_bonuses WHERE user_id = $1;

-- name: UpsertFacilityBonus :exec
INSERT INTO industry_facility_bonuses (user_id, facility_id, material_bonus, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (user_id, facility_id) DO UPDATE SET material_bonus = excluded.material_bonus, updated_at = excluded.updated_at;

-- name: DeleteFacilityBonus :exec
DELETE FROM industry_facility_bonuses WHERE user_id = $1 AND facility_id = $2;

-- name: ListPoolFacilities :many
-- The facilities a pool's jobs ran in, most used first.
SELECT facility_id, COUNT(*)::bigint AS jobs, MAX(at)::timestamptz AS last_at
FROM ledger_jobs
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND facility_id > 0
GROUP BY facility_id
ORDER BY COUNT(*) DESC, facility_id
LIMIT 20;

-- name: ListJobsAtFacility :many
-- Jobs whose derived materials a changed facility bonus rewrites.
SELECT owner_kind, owner_id, job_id, blueprint_type_id, runs, me
FROM ledger_jobs
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND facility_id = sqlc.arg(facility_id) AND blueprint_type_id > 0;

-- name: SetJobBonus :exec
UPDATE ledger_jobs SET material_bonus = $4, bonus_source = $5
WHERE owner_kind = $1 AND owner_id = $2 AND job_id = $3;

-- name: UpdateJobMaterialQuantity :exec
UPDATE ledger_job_materials SET quantity = $5
WHERE owner_kind = $1 AND owner_id = $2 AND job_id = $3 AND type_id = $4;

-- name: RecomputeJobInputsAtTime :exec
UPDATE ledger_jobs j SET inputs_at_time = COALESCE((
    SELECT SUM(m.quantity * m.unit_at_time) FROM ledger_job_materials m
    WHERE m.owner_kind = j.owner_kind AND m.owner_id = j.owner_id AND m.job_id = j.job_id), 0)
WHERE j.owner_kind = $1 AND j.owner_id = $2 AND j.job_id = $3;
