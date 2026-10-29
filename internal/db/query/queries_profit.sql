-- Profit ledger (schema 104): every journal row, transaction and
-- delivered job seen per owner, and the attribution written from it.

-- name: GetLedgerState :one
SELECT owner_kind, owner_id, journal_at, txns_at, jobs_at, journal_hash, txns_hash, jobs_hash, folded_at
FROM ledger_state WHERE owner_kind = $1 AND owner_id = $2;

-- name: UpsertLedgerState :exec
INSERT INTO ledger_state (owner_kind, owner_id, journal_at, txns_at, jobs_at, journal_hash, txns_hash, jobs_hash, folded_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (owner_kind, owner_id) DO UPDATE SET
    journal_at = excluded.journal_at, txns_at = excluded.txns_at,
    jobs_at = excluded.jobs_at, journal_hash = excluded.journal_hash,
    txns_hash = excluded.txns_hash, jobs_hash = excluded.jobs_hash,
    folded_at = excluded.folded_at;

-- name: InsertLedgerJournalBatch :many
-- New rows only; returns the days they fall on so the fold knows
-- where to start.
INSERT INTO ledger_journal (owner_kind, owner_id, entry_id, at, ref_type, activity, amount, first_party_id, second_party_id, context_id, context_type)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint,
       unnest(sqlc.arg(entry_ids)::bigint[]), unnest(sqlc.arg(ats)::text[])::timestamptz,
       unnest(sqlc.arg(ref_types)::text[]), unnest(sqlc.arg(activities)::text[]),
       unnest(sqlc.arg(amounts)::float8[]), unnest(sqlc.arg(first_parties)::bigint[]),
       unnest(sqlc.arg(second_parties)::bigint[]), unnest(sqlc.arg(context_ids)::bigint[]),
       unnest(sqlc.arg(context_types)::text[])
ON CONFLICT DO NOTHING
RETURNING at;

-- name: InsertLedgerTxnBatch :many
INSERT INTO ledger_txns (owner_kind, owner_id, transaction_id, at, type_id, quantity, unit_price, is_buy, location_id, journal_ref_id)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint,
       unnest(sqlc.arg(transaction_ids)::bigint[]), unnest(sqlc.arg(ats)::text[])::timestamptz,
       unnest(sqlc.arg(type_ids)::bigint[]), unnest(sqlc.arg(quantities)::bigint[]),
       unnest(sqlc.arg(unit_prices)::float8[]), unnest(sqlc.arg(is_buys)::bool[]),
       unnest(sqlc.arg(location_ids)::bigint[]), unnest(sqlc.arg(journal_ref_ids)::bigint[])
ON CONFLICT DO NOTHING
RETURNING at, type_id;

-- name: ListLedgerJobIDs :many
-- Jobs already landed with their recipe facts.
SELECT job_id FROM ledger_jobs
WHERE owner_kind = $1 AND owner_id = $2 AND job_id = ANY(sqlc.arg(job_ids)::bigint[]) AND blueprint_type_id > 0;


-- name: GetUserProfitPrefs :one
SELECT pooled, broker_in_margin, industry_region, industry_side, lp_isk_value FROM user_profit_prefs WHERE user_id = $1;

-- name: UpsertUserProfitPrefs :exec
INSERT INTO user_profit_prefs (user_id, pooled, broker_in_margin, industry_region, industry_side, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (user_id) DO UPDATE SET
    pooled = excluded.pooled, broker_in_margin = excluded.broker_in_margin,
    industry_region = excluded.industry_region, industry_side = excluded.industry_side,
    updated_at = excluded.updated_at;

-- name: GetLedgerPoolState :one
SELECT pool_kind, pool_id, members, folded_at, refolded_at, refold_reason
FROM ledger_pool_state WHERE pool_kind = $1 AND pool_id = $2;

-- name: UpsertLedgerPoolState :exec
INSERT INTO ledger_pool_state (pool_kind, pool_id, members, folded_at, refolded_at, refold_reason)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (pool_kind, pool_id) DO UPDATE SET
    members = excluded.members, folded_at = excluded.folded_at,
    refolded_at = excluded.refolded_at, refold_reason = excluded.refold_reason;

-- name: ClearLedgerPoolMembers :exec
-- Forces the pool to refold from scratch on its next pass (a manual
-- cost basis was added).
UPDATE ledger_pool_state SET members = '' WHERE pool_kind = $1 AND pool_id = $2;

-- name: InsertLedgerHole :exec
INSERT INTO ledger_holes (owner_kind, owner_id, from_at, to_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: ListLedgerHoles :many
SELECT from_at, to_at FROM ledger_holes
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
ORDER BY from_at;

-- name: GetLedgerCoverageStart :one
-- The earliest moment the ledger holds anything for these owners.
SELECT COALESCE(LEAST(
    (SELECT MIN(at) FROM ledger_journal j WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])),
    (SELECT MIN(at) FROM ledger_txns t WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]))
), now())::timestamptz AS start;

-- name: InsertLedgerOrderIssues :exec
INSERT INTO ledger_order_issues (owner_kind, owner_id, order_id, issued, type_id)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint,
       unnest(sqlc.arg(order_ids)::bigint[]), unnest(sqlc.arg(issueds)::text[])::timestamptz,
       unnest(sqlc.arg(type_ids)::bigint[])
ON CONFLICT DO NOTHING;

-- name: DeleteSaleTaxFrom :exec
DELETE FROM ledger_sale_tax
WHERE owner_kind = $1 AND owner_id = $2 AND at >= sqlc.arg(from_at)::timestamptz;

-- name: FoldSaleTax :exec
-- Sales tax lines never name their transaction (context_id is 0), so
-- they pair by time: an owner's sells and tax lines are grouped by the
-- exact second, the group's tax is split across its sells pro rata by
-- sale amount, and the group passes only when its rate agrees with a
-- neighbouring group's (the rate is uniform at any moment and moves
-- only when Accounting trains). A lone group passes on a plausible
-- rate. Sells with no tax in their second, or in a failing group, are
-- stored unpaired.
WITH sells AS (
    SELECT t.transaction_id, t.at, date_trunc('second', t.at) AS sec, t.quantity * t.unit_price AS rev
    FROM ledger_txns t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = sqlc.arg(owner_id) AND NOT t.is_buy
      AND t.at >= sqlc.arg(from_at)::timestamptz - interval '7 days'
), grouped AS (
    SELECT sec, SUM(rev) AS rev FROM sells GROUP BY sec
), taxes AS (
    SELECT date_trunc('second', j.at) AS sec, SUM(-j.amount) AS tax
    FROM ledger_journal j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = sqlc.arg(owner_id) AND j.ref_type = 'transaction_tax'
      AND j.at >= sqlc.arg(from_at)::timestamptz - interval '7 days'
    GROUP BY 1
), rated AS (
    SELECT g.sec, g.rev, x.tax, x.tax / NULLIF(g.rev, 0) AS rate
    FROM grouped g JOIN taxes x ON x.sec = g.sec
), checked AS (
    SELECT sec, rev, tax, rate,
           LAG(rate) OVER w AS prev_rate, LEAD(rate) OVER w AS next_rate,
           0.0005 + 0.02 / NULLIF(rev, 0) AS tol
    FROM rated
    WINDOW w AS (ORDER BY sec)
)
INSERT INTO ledger_sale_tax (owner_kind, owner_id, transaction_id, at, tax, paired)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, s.transaction_id, s.at,
       COALESCE(c.tax * s.rev / NULLIF(c.rev, 0), 0)::float8,
       COALESCE(c.rate > 0 AND c.rate <= 0.15 AND (
           (c.prev_rate IS NULL AND c.next_rate IS NULL)
           OR abs(c.rate - c.prev_rate) <= c.tol OR abs(c.rate - c.next_rate) <= c.tol), false)
FROM sells s
LEFT JOIN checked c ON c.sec = s.sec
WHERE s.at >= sqlc.arg(from_at)::timestamptz
ON CONFLICT (owner_kind, owner_id, transaction_id) DO UPDATE SET
    at = excluded.at, tax = excluded.tax, paired = excluded.paired;

-- name: DeleteBrokerAllocFrom :exec
DELETE FROM ledger_broker_alloc
WHERE owner_kind = $1 AND owner_id = $2 AND at >= sqlc.arg(from_at)::timestamptz;

-- name: FoldBrokerAlloc :exec
-- Broker fee lines carry no order id: a fee is tied to an order's type
-- only when exactly one of the owner's orders was placed or repriced
-- within a minute of it. The rest stay period-level.
INSERT INTO ledger_broker_alloc (owner_kind, owner_id, entry_id, at, order_id, type_id, amount)
SELECT j.owner_kind, j.owner_id, j.entry_id, j.at, c.order_id, c.type_id, -j.amount
FROM ledger_journal j
CROSS JOIN LATERAL (
    SELECT MIN(o.order_id)::bigint AS order_id, MIN(o.type_id)::bigint AS type_id, COUNT(DISTINCT o.order_id) AS n
    FROM ledger_order_issues o
    WHERE o.owner_kind = j.owner_kind AND o.owner_id = j.owner_id
      AND o.issued BETWEEN j.at - interval '60 seconds' AND j.at + interval '60 seconds'
) c
WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = sqlc.arg(owner_id)
  AND j.ref_type = 'brokers_fee' AND j.at >= sqlc.arg(from_at)::timestamptz AND c.n = 1
ON CONFLICT (owner_kind, owner_id, entry_id) DO UPDATE SET
    at = excluded.at, order_id = excluded.order_id, type_id = excluded.type_id, amount = excluded.amount;

-- name: ListPoolLotsForTypeSince :many
-- Everything that adds or removes stock of one type across a pool's
-- members, oldest first, from a fold checkpoint on:
--   'txn'     market buys and sells, with the member that made them
--             (a sell the player linked to a job, or one from an order the
--             player watches for a job, prefers that job's lot);
--   'job'     delivered job output, at the house price the day it delivered;
--   'basis'   the pool's manual cost basis entries;
--   'consume' materials a job used, removed when it was installed;
--   'msale', 'lost' units of a job's output the player sold off-market
--             or marked lost.
--   'lp'      units of an LP store purchase, at its ISK and required items
--             (LP at 0), from the start of the bracket it was bought in;
--   'lpreq'   items an LP store purchase took in exchange.
--   'contract' items sold on a completed item exchange contract, at their
--             share of its price.
-- The caller prepends the stored open lots at the checkpoint, which
-- stand in for everything older.
SELECT x.at, x.ref_id, x.owner_id, x.is_sell, x.quantity, x.unit_price, x.kind, x.prefer_ref
FROM (
    SELECT t.at, t.transaction_id AS ref_id, t.owner_id, NOT t.is_buy AS is_sell, t.quantity, t.unit_price, 'txn' AS kind,
           COALESCE((SELECT -e.job_id FROM job_manual_events e
                     WHERE e.pool_kind = sqlc.arg(pool_kind) AND e.pool_id = sqlc.arg(pool_id)
                       AND e.kind = 'link' AND e.transaction_id = t.transaction_id
                     ORDER BY e.id DESC LIMIT 1),
                    (SELECT -w.job_id FROM job_order_watches w
                     JOIN order_lifecycle o ON o.character_id = w.character_id AND o.order_id = w.order_id
                     WHERE w.pool_kind = sqlc.arg(pool_kind) AND w.pool_id = sqlc.arg(pool_id)
                       AND t.owner_kind = 'char' AND w.character_id = t.owner_id AND o.type_id = t.type_id
                       AND o.location_id = t.location_id AND t.at >= w.created_at
                       AND (o.closed_at IS NULL OR t.at <= o.closed_at + interval '1 hour')
                     ORDER BY w.id DESC LIMIT 1), 0)::bigint AS prefer_ref
    FROM ledger_txns t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND t.type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT j.at, -j.job_id, j.owner_id, false, j.units,
           CASE WHEN j.units > 0 THEN j.output_value / j.units ELSE 0 END, 'job', 0::bigint
    FROM ledger_jobs j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
      AND j.product_type_id = sqlc.arg(type_id) AND j.priced
    UNION ALL
    SELECT m.at, -(4000000000000000 + m.id), 0::bigint, false, m.quantity, m.unit_cost, 'basis', 0::bigint
    FROM ledger_cost_basis_manual m
    WHERE m.pool_kind = sqlc.arg(pool_kind) AND m.pool_id = sqlc.arg(pool_id) AND m.type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT COALESCE(j.installed_at, j.at), -j.job_id, j.owner_id, true, m.quantity, 0::float8, 'consume', 0::bigint
    FROM ledger_job_materials m
    JOIN ledger_jobs j ON j.owner_kind = m.owner_kind AND j.owner_id = m.owner_id AND j.job_id = m.job_id
    WHERE m.owner_kind = sqlc.arg(owner_kind) AND m.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND m.type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT e.at, e.id, j.owner_id, true, e.units, e.unit_price, CASE WHEN e.kind = 'sale' THEN 'msale' ELSE 'lost' END, -e.job_id
    FROM job_manual_events e
    JOIN ledger_jobs j ON j.job_id = e.job_id AND j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
    WHERE e.pool_kind = sqlc.arg(pool_kind) AND e.pool_id = sqlc.arg(pool_id) AND e.kind IN ('sale', 'lost')
      AND j.product_type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT c.at, c.contract_id, c.owner_id, true, c.quantity, c.price_share / NULLIF(c.quantity, 0), 'contract', 0::bigint
    FROM ledger_contract_items c
    WHERE c.owner_kind = sqlc.arg(owner_kind) AND c.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND c.type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT p.from_at, -(5000000000000000 + p.id), p.character_id, false, p.units,
           (p.isk_paid + p.req_value) / NULLIF(p.units, 0), 'lp', 0::bigint
    FROM lp_purchases p
    WHERE sqlc.arg(owner_kind)::text = 'char' AND p.character_id = ANY(sqlc.arg(owner_ids)::bigint[])
      AND p.product_type_id = sqlc.arg(type_id) AND p.units > 0 AND p.tier IN ('corroborated', 'inferred', 'user_confirmed')
    UNION ALL
    SELECT p.from_at, -(5000000000000000 + p.id), p.character_id, true, r.quantity * p.quantity, 0::float8, 'lpreq', 0::bigint
    FROM lp_purchases p
    JOIN lp_offer_requirements r ON r.corporation_id = p.corporation_id AND r.offer_id = p.offer_id
    WHERE sqlc.arg(owner_kind)::text = 'char' AND p.character_id = ANY(sqlc.arg(owner_ids)::bigint[])
      AND r.type_id = sqlc.arg(type_id) AND p.tier IN ('corroborated', 'inferred', 'user_confirmed')
) x
WHERE x.at >= sqlc.arg(since)::timestamptz
ORDER BY x.at, x.ref_id;

-- name: ListPoolTypes :many
-- Every type a pool has bought, sold, built or entered a basis for.
SELECT DISTINCT type_id FROM (
    SELECT t.type_id FROM ledger_txns t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
    UNION
    SELECT j.product_type_id FROM ledger_jobs j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND j.priced
    UNION
    SELECT c.type_id FROM ledger_contract_items c
    WHERE c.owner_kind = sqlc.arg(owner_kind) AND c.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
    UNION
    SELECT p.product_type_id FROM lp_purchases p
    WHERE sqlc.arg(owner_kind)::text = 'char' AND p.character_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND p.product_type_id > 0
    UNION
    SELECT m.type_id FROM ledger_job_materials m
    WHERE m.owner_kind = sqlc.arg(owner_kind) AND m.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
    UNION
    SELECT m.type_id FROM ledger_cost_basis_manual m
    WHERE m.pool_kind = sqlc.arg(pool_kind) AND m.pool_id = sqlc.arg(pool_id)
) x
ORDER BY type_id;

-- name: ListSoldTypesSince :many
-- Types an owner sold from a moment on: their realized rows follow a
-- change of paired tax, so the item rollups refold from there.
SELECT DISTINCT type_id FROM ledger_txns
WHERE owner_kind = $1 AND owner_id = $2 AND NOT is_buy AND at >= sqlc.arg(since)::timestamptz;

-- name: DeleteTradeRealizedForPool :exec
-- A pool refolding from scratch: its own rows and any its members left
-- under an earlier pool.
DELETE FROM trade_realized
WHERE (pool_kind = sqlc.arg(pool_kind) AND pool_id = sqlc.arg(pool_id))
   OR (owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]));

-- name: DeletePoolCheckpoints :exec
-- A pool key's checkpoints, open lots and positions, cleared before
-- it refolds from scratch.
DELETE FROM trade_fold_checkpoints WHERE owner_kind = sqlc.arg(pool_kind) AND owner_id = sqlc.arg(pool_id);

-- name: DeletePoolOpenLots :exec
DELETE FROM trade_open_lots WHERE owner_kind = sqlc.arg(pool_kind) AND owner_id = sqlc.arg(pool_id);

-- name: DeletePoolPositions :exec
DELETE FROM trade_positions WHERE owner_kind = sqlc.arg(pool_kind) AND owner_id = sqlc.arg(pool_id);

-- name: DeleteTradeRealizedForPoolTypeFrom :exec
DELETE FROM trade_realized
WHERE pool_kind = sqlc.arg(pool_kind) AND pool_id = sqlc.arg(pool_id) AND type_id = sqlc.arg(type_id)
  AND day >= sqlc.arg(from_day)::text;

-- name: InsertTradeRealizedBatch :exec
INSERT INTO trade_realized (owner_kind, owner_id, transaction_id, day, type_id, quantity, revenue, cost,
                            unmatched_qty, unmatched_revenue, planetary, pool_kind, pool_id, gap_kind)
SELECT sqlc.arg(owner_kind)::text, unnest(sqlc.arg(owner_ids)::bigint[]),
       unnest(sqlc.arg(transaction_ids)::bigint[]), unnest(sqlc.arg(days)::text[]),
       sqlc.arg(type_id)::bigint,
       unnest(sqlc.arg(quantities)::bigint[]), unnest(sqlc.arg(revenues)::float8[]),
       unnest(sqlc.arg(costs)::float8[]), unnest(sqlc.arg(unmatched_qtys)::bigint[]),
       unnest(sqlc.arg(unmatched_revenues)::float8[]), sqlc.arg(planetary)::bool,
       sqlc.arg(pool_kind)::text, sqlc.arg(pool_id)::bigint, unnest(sqlc.arg(gap_kinds)::text[])
ON CONFLICT (owner_kind, owner_id, transaction_id) DO UPDATE SET
    quantity = excluded.quantity, revenue = excluded.revenue, cost = excluded.cost,
    unmatched_qty = excluded.unmatched_qty, unmatched_revenue = excluded.unmatched_revenue,
    planetary = excluded.planetary, pool_kind = excluded.pool_kind, pool_id = excluded.pool_id,
    gap_kind = excluded.gap_kind;

-- name: DeleteTradeItemDailyFrom :exec
DELETE FROM trade_item_daily WHERE owner_kind = $1 AND owner_id = $2 AND day >= sqlc.arg(from_day)::text;

-- name: FoldTradeItemDaily :exec
-- Per seller, day and type from the day on: units and revenue sold,
-- the covered part (matched units of sales whose tax paired) with its
-- cost and tax share, broker fees tied to the type, and the units
-- left uncovered by reason.
INSERT INTO trade_item_daily (owner_kind, owner_id, day, type_id, units, revenue, cov_units, cov_revenue, cov_cost,
                              cov_tax, broker, gap_before_history, gap_ledger_hole, gap_no_buy, gap_no_tax)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, day, type_id,
       SUM(units), SUM(revenue), SUM(cov_units), SUM(cov_revenue), SUM(cov_cost), SUM(cov_tax), SUM(broker),
       SUM(g_before), SUM(g_hole), SUM(g_buy), SUM(g_tax)
FROM (
    SELECT r.day, r.type_id,
           r.quantity + r.unmatched_qty AS units, r.revenue + r.unmatched_revenue AS revenue,
           CASE WHEN s.paired THEN r.quantity ELSE 0 END AS cov_units,
           CASE WHEN s.paired THEN r.revenue ELSE 0 END AS cov_revenue,
           CASE WHEN s.paired THEN r.cost ELSE 0 END AS cov_cost,
           CASE WHEN s.paired AND r.quantity + r.unmatched_qty > 0
                THEN s.tax * r.quantity / (r.quantity + r.unmatched_qty) ELSE 0 END AS cov_tax,
           0::float8 AS broker,
           CASE WHEN r.gap_kind = 'before_history' THEN r.unmatched_qty ELSE 0 END AS g_before,
           CASE WHEN r.gap_kind = 'ledger_hole' THEN r.unmatched_qty ELSE 0 END AS g_hole,
           CASE WHEN r.gap_kind = 'no_buy' THEN r.unmatched_qty ELSE 0 END AS g_buy,
           CASE WHEN COALESCE(s.paired, false) THEN 0 ELSE r.quantity END AS g_tax
    FROM trade_realized r
    LEFT JOIN ledger_sale_tax s ON s.owner_kind = r.owner_kind AND s.owner_id = r.owner_id AND s.transaction_id = r.transaction_id
    WHERE r.owner_kind = sqlc.arg(owner_kind) AND r.owner_id = sqlc.arg(owner_id) AND r.day >= sqlc.arg(from_day)::text
    UNION ALL
    SELECT to_char(b.at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), b.type_id, 0, 0, 0, 0, 0, 0, b.amount, 0, 0, 0, 0
    FROM ledger_broker_alloc b
    WHERE b.owner_kind = sqlc.arg(owner_kind) AND b.owner_id = sqlc.arg(owner_id)
      AND b.at >= sqlc.arg(from_day)::date::timestamptz
) x
GROUP BY day, type_id;

-- name: GetTradeItemWindowAsOf :one
SELECT COALESCE(MAX(as_of_day), '')::text FROM trade_item_window WHERE owner_kind = $1 AND owner_id = $2;

-- name: DeleteTradeItemWindows :exec
DELETE FROM trade_item_window WHERE owner_kind = $1 AND owner_id = $2;

-- name: FoldTradeItemWindows :exec
-- The daily rows summed over the last 7, 30, 90 and 365 days as of a
-- day, with the previous window of equal length beside them.
INSERT INTO trade_item_window (owner_kind, owner_id, win, type_id, as_of_day, units, revenue, cov_units, cov_revenue,
                               cov_cost, cov_tax, broker, gap_before_history, gap_ledger_hole, gap_no_buy, gap_no_tax,
                               prev_cov_net, prev_broker, prev_units, prev_cov_units)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, w.win, d.type_id, sqlc.arg(as_of_day)::text,
       SUM(CASE WHEN cur THEN d.units ELSE 0 END), SUM(CASE WHEN cur THEN d.revenue ELSE 0 END),
       SUM(CASE WHEN cur THEN d.cov_units ELSE 0 END), SUM(CASE WHEN cur THEN d.cov_revenue ELSE 0 END),
       SUM(CASE WHEN cur THEN d.cov_cost ELSE 0 END), SUM(CASE WHEN cur THEN d.cov_tax ELSE 0 END),
       SUM(CASE WHEN cur THEN d.broker ELSE 0 END),
       SUM(CASE WHEN cur THEN d.gap_before_history ELSE 0 END), SUM(CASE WHEN cur THEN d.gap_ledger_hole ELSE 0 END),
       SUM(CASE WHEN cur THEN d.gap_no_buy ELSE 0 END), SUM(CASE WHEN cur THEN d.gap_no_tax ELSE 0 END),
       SUM(CASE WHEN cur THEN 0 ELSE d.cov_revenue - d.cov_cost - d.cov_tax END),
       SUM(CASE WHEN cur THEN 0 ELSE d.broker END),
       SUM(CASE WHEN cur THEN 0 ELSE d.units END), SUM(CASE WHEN cur THEN 0 ELSE d.cov_units END)
FROM (VALUES (7), (30), (90), (365)) AS w (win)
JOIN LATERAL (
    SELECT t.*, t.day >= to_char(sqlc.arg(as_of_day)::date - (w.win - 1), 'YYYY-MM-DD') AS cur
    FROM trade_item_daily t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = sqlc.arg(owner_id)
      AND t.day >= to_char(sqlc.arg(as_of_day)::date - (2 * w.win - 1), 'YYYY-MM-DD')
      AND t.day <= sqlc.arg(as_of_day)::text
) d ON true
GROUP BY w.win, d.type_id
HAVING SUM(CASE WHEN cur THEN d.units ELSE 0 END) > 0 OR SUM(CASE WHEN cur THEN d.broker ELSE 0 END) > 0;

-- name: ListTradeItems :many
-- The Trading tab: per type over a window for a set of sellers, fully
-- covered items first, then by covered margin. Broker fees count in
-- the margin unless the player set them aside.
SELECT type_id,
       SUM(units)::bigint AS units, SUM(revenue)::float8 AS revenue,
       SUM(cov_units)::bigint AS cov_units, SUM(cov_revenue)::float8 AS cov_revenue,
       SUM(cov_cost)::float8 AS cov_cost, SUM(cov_tax)::float8 AS cov_tax, SUM(broker)::float8 AS broker,
       SUM(gap_before_history)::bigint AS gap_before_history, SUM(gap_ledger_hole)::bigint AS gap_ledger_hole,
       SUM(gap_no_buy)::bigint AS gap_no_buy, SUM(gap_no_tax)::bigint AS gap_no_tax,
       SUM(prev_cov_net)::float8 AS prev_cov_net, SUM(prev_broker)::float8 AS prev_broker,
       SUM(prev_units)::bigint AS prev_units, SUM(prev_cov_units)::bigint AS prev_cov_units
FROM trade_item_window
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND win = sqlc.arg(win)::int
GROUP BY type_id
ORDER BY (SUM(units) > 0 AND SUM(cov_units) >= SUM(units)) DESC,
         SUM(cov_revenue - cov_cost - cov_tax - CASE WHEN sqlc.arg(broker_in_margin)::bool THEN broker ELSE 0 END) DESC,
         type_id
LIMIT sqlc.arg(row_cap)::bigint;

-- name: SumTradeItems :one
-- The Trading tab's header: the same figures over every type.
SELECT COALESCE(SUM(units), 0)::bigint AS units, COALESCE(SUM(cov_units), 0)::bigint AS cov_units,
       COALESCE(SUM(cov_revenue - cov_cost - cov_tax), 0)::float8 AS cov_net,
       COALESCE(SUM(broker), 0)::float8 AS broker,
       COALESCE(SUM(prev_cov_net), 0)::float8 AS prev_cov_net, COALESCE(SUM(prev_broker), 0)::float8 AS prev_broker,
       COALESCE(SUM(prev_units), 0)::bigint AS prev_units, COALESCE(SUM(prev_cov_units), 0)::bigint AS prev_cov_units,
       COALESCE(MAX(as_of_day), '')::text AS as_of_day
FROM trade_item_window
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND win = sqlc.arg(win)::int;

-- name: InsertManualCostBasis :exec
INSERT INTO ledger_cost_basis_manual (pool_kind, pool_id, type_id, at, quantity, unit_cost, note)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetFirstUncoveredSale :one
-- When a pool first sold units of a type that no lot covered: a
-- manual basis is dated just before it.
SELECT COALESCE(MIN(t.at), now())::timestamptz AS at
FROM trade_realized r
JOIN ledger_txns t ON t.owner_kind = r.owner_kind AND t.owner_id = r.owner_id AND t.transaction_id = r.transaction_id
WHERE r.pool_kind = sqlc.arg(pool_kind) AND r.pool_id = sqlc.arg(pool_id) AND r.type_id = sqlc.arg(type_id) AND r.unmatched_qty > 0;
-- name: GetTradeFoldCheckpoint :one
SELECT checkpoint_at FROM trade_fold_checkpoints
WHERE owner_kind = $1 AND owner_id = $2 AND type_id = $3;

-- name: UpsertTradeFoldCheckpoint :exec
INSERT INTO trade_fold_checkpoints (owner_kind, owner_id, type_id, checkpoint_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (owner_kind, owner_id, type_id) DO UPDATE SET
    checkpoint_at = excluded.checkpoint_at;

-- name: ListTradeOpenLots :many
SELECT quantity, unit_price, ref_id FROM trade_open_lots
WHERE owner_kind = $1 AND owner_id = $2 AND type_id = $3
ORDER BY seq;

-- name: DeleteTradeOpenLots :exec
DELETE FROM trade_open_lots WHERE owner_kind = $1 AND owner_id = $2 AND type_id = $3;

-- name: InsertTradeOpenLotsBatch :exec
INSERT INTO trade_open_lots (owner_kind, owner_id, type_id, seq, quantity, unit_price, ref_id)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, sqlc.arg(type_id)::bigint,
       unnest(sqlc.arg(seqs)::bigint[]), unnest(sqlc.arg(quantities)::bigint[]),
       unnest(sqlc.arg(unit_prices)::float8[]), unnest(sqlc.arg(ref_ids)::bigint[]);

-- name: UpsertTradePosition :exec
INSERT INTO trade_positions (owner_kind, owner_id, type_id, quantity, cost, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_kind, owner_id, type_id) DO UPDATE SET
    quantity = excluded.quantity, cost = excluded.cost, updated_at = excluded.updated_at;

-- name: DeleteTradePosition :exec
DELETE FROM trade_positions WHERE owner_kind = $1 AND owner_id = $2 AND type_id = $3;

-- name: IsPlanetaryType :one
SELECT EXISTS (
    SELECT 1 FROM sde_types t JOIN sde_groups g ON g.group_id = t.group_id
    WHERE t.type_id = $1 AND g.category_id IN (42, 43)
);

-- name: DeleteProfitDailyFrom :exec
DELETE FROM profit_daily WHERE owner_kind = $1 AND owner_id = $2 AND day >= sqlc.arg(from_day)::text;

-- name: FoldProfitDaily :exec
-- Income and costs per day and activity from the day on: journal
-- rows by their activity (market buys and sells, escrow and the
-- like excluded: the transactions speak for trade), realized trades
-- (trading), unmatched sales (planetary industry or other sales),
-- and delivered jobs (industry: output value less materials).
INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs)
SELECT sqlc.arg(owner_kind)::text, sqlc.arg(owner_id)::bigint, day, activity, SUM(income), SUM(costs)
FROM (
    SELECT to_char(j.at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, j.activity,
           GREATEST(j.amount, 0) AS income, GREATEST(-j.amount, 0) AS costs
    FROM ledger_journal j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = sqlc.arg(owner_id)
      AND j.at >= sqlc.arg(from_at)::timestamptz AND j.activity NOT IN ('market', 'escrow', 'lp_store')
    UNION ALL
    SELECT r.day, 'trading', r.revenue, r.cost
    FROM trade_realized r
    WHERE r.owner_kind = sqlc.arg(owner_kind) AND r.owner_id = sqlc.arg(owner_id)
      AND r.day >= sqlc.arg(from_day)::text AND r.quantity > 0
    UNION ALL
    SELECT r.day, CASE WHEN r.planetary THEN 'pi' ELSE 'sales' END, r.unmatched_revenue, 0
    FROM trade_realized r
    WHERE r.owner_kind = sqlc.arg(owner_kind) AND r.owner_id = sqlc.arg(owner_id)
      AND r.day >= sqlc.arg(from_day)::text AND r.unmatched_qty > 0
    UNION ALL
    SELECT to_char(b.at AT TIME ZONE 'UTC', 'YYYY-MM-DD'), 'industry', b.output_value, b.materials_cost
    FROM ledger_jobs b
    WHERE b.owner_kind = sqlc.arg(owner_kind) AND b.owner_id = sqlc.arg(owner_id)
      AND b.at >= sqlc.arg(from_at)::timestamptz AND b.priced
) x
GROUP BY day, activity;

-- name: FoldProfitRemainder :exec
-- A character's unattributed remainder per day: the net worth change
-- since its previous sampled day, less every attributed activity
-- that day. Days without both net worth samples get no row.
INSERT INTO profit_daily (owner_kind, owner_id, day, activity, income, costs)
SELECT 'char', sqlc.arg(character_id)::bigint, w.day, 'unattributed',
       GREATEST(w.change - COALESCE(a.net, 0), 0), GREATEST(COALESCE(a.net, 0) - w.change, 0)
FROM (
    SELECT h.day, h.net_worth - LAG(h.net_worth) OVER (ORDER BY h.day) AS change
    FROM wallet_history h
    WHERE h.character_id = sqlc.arg(character_id)
) w
LEFT JOIN (
    SELECT p.day, SUM(p.income - p.costs) AS net FROM profit_daily p
    WHERE p.owner_kind = 'char' AND p.owner_id = sqlc.arg(character_id) AND p.activity <> 'unattributed'
    GROUP BY p.day
) a ON a.day = w.day
WHERE w.change IS NOT NULL AND w.day >= sqlc.arg(from_day)::text
ON CONFLICT (owner_kind, owner_id, day, activity) DO UPDATE SET
    income = excluded.income, costs = excluded.costs;

-- name: DeleteProfitRemainderFrom :exec
DELETE FROM profit_daily
WHERE owner_kind = 'char' AND owner_id = $1 AND activity = 'unattributed' AND day >= sqlc.arg(from_day)::text;

-- name: ListProfitByActivity :many
-- The profit page's attribution table: per activity over a span,
-- for a set of owners of one kind.
SELECT activity, SUM(income)::float8 AS income, SUM(costs)::float8 AS costs, COUNT(DISTINCT day) AS days
FROM profit_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND day >= sqlc.arg(from_day)::text
GROUP BY activity
ORDER BY activity;

-- name: ListProfitDaily :many
-- Net per day for the chart, attributed and unattributed apart.
SELECT day,
       SUM(CASE WHEN activity <> 'unattributed' THEN income - costs ELSE 0 END)::float8 AS attributed,
       SUM(CASE WHEN activity = 'unattributed' THEN income - costs ELSE 0 END)::float8 AS unattributed
FROM profit_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND day >= sqlc.arg(from_day)::text
GROUP BY day
ORDER BY day
LIMIT 400;

-- name: ListTradePositions :many
SELECT type_id, SUM(quantity)::bigint AS quantity, SUM(cost)::float8 AS cost
FROM trade_positions
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND quantity > 0
GROUP BY type_id
ORDER BY SUM(cost) DESC, type_id
LIMIT 100;

-- name: SumTradingFeesFrom :one
-- Trading fees over the span, for the trading line's breakdown.
SELECT COALESCE(SUM(CASE WHEN ref_type = 'transaction_tax' THEN -amount ELSE 0 END), 0)::float8 AS sales_tax,
       COALESCE(SUM(CASE WHEN ref_type = 'brokers_fee' THEN -amount ELSE 0 END), 0)::float8 AS broker_fees
FROM ledger_journal
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND at >= sqlc.arg(from_at)::timestamptz AND activity = 'trading';

-- name: ListPositionCostsForTypes :many
-- Open position cost per unit for a pool's types: the basis a beaten
-- sell order's margin is measured against.
SELECT type_id, quantity, cost FROM trade_positions
WHERE owner_kind = $1 AND owner_id = $2 AND type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND quantity > 0;

-- name: GetObservedSalesTaxRate :one
-- A character's effective sales tax rate over a span: transaction
-- tax paid over market sell revenue in the ledger.
SELECT COALESCE((SELECT SUM(-j.amount) FROM ledger_journal j
                 WHERE j.owner_kind = 'char' AND j.owner_id = sqlc.arg(character_id)::bigint
                   AND j.ref_type = 'transaction_tax' AND j.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS tax,
       COALESCE((SELECT SUM(t.quantity * t.unit_price) FROM ledger_txns t
                 WHERE t.owner_kind = 'char' AND t.owner_id = sqlc.arg(character_id)::bigint
                   AND NOT t.is_buy AND t.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS revenue;
