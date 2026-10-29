-- Profit ledger (schema 104): every journal row, transaction and
-- delivered job seen per owner, and the attribution written from it.

-- name: GetLedgerState :one
SELECT owner_kind, owner_id, journal_at, txns_at, jobs_at, folded_at
FROM ledger_state WHERE owner_kind = $1 AND owner_id = $2;

-- name: UpsertLedgerState :exec
INSERT INTO ledger_state (owner_kind, owner_id, journal_at, txns_at, jobs_at, folded_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_kind, owner_id) DO UPDATE SET
    journal_at = excluded.journal_at, txns_at = excluded.txns_at,
    jobs_at = excluded.jobs_at, folded_at = excluded.folded_at;

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

-- name: InsertLedgerJob :one
INSERT INTO ledger_jobs (owner_kind, owner_id, job_id, at, activity_id, product_type_id, units, output_value, materials_cost, priced)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT DO NOTHING
RETURNING at;

-- name: ListLedgerJobIDs :many
SELECT job_id FROM ledger_jobs
WHERE owner_kind = $1 AND owner_id = $2 AND job_id = ANY(sqlc.arg(job_ids)::bigint[]);

-- name: ListLedgerLotsForType :many
-- Everything that adds or removes stock of one type for one owner,
-- oldest first: market buys and sells, and delivered job output
-- (valued at the house price the day it delivered).
SELECT x.at, x.ref_id, x.is_sell, x.quantity, x.unit_price
FROM (
    SELECT t.at, t.transaction_id AS ref_id, NOT t.is_buy AS is_sell, t.quantity, t.unit_price
    FROM ledger_txns t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = sqlc.arg(owner_id) AND t.type_id = sqlc.arg(type_id)
    UNION ALL
    SELECT j.at, -j.job_id AS ref_id, false AS is_sell, j.units AS quantity,
           CASE WHEN j.units > 0 THEN j.output_value / j.units ELSE 0 END AS unit_price
    FROM ledger_jobs j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = sqlc.arg(owner_id)
      AND j.product_type_id = sqlc.arg(type_id) AND j.priced
) x
ORDER BY x.at, x.ref_id;

-- name: DeleteTradeRealizedForType :exec
DELETE FROM trade_realized WHERE owner_kind = $1 AND owner_id = $2 AND type_id = $3;

-- name: InsertTradeRealized :exec
INSERT INTO trade_realized (owner_kind, owner_id, transaction_id, day, type_id, quantity, revenue, cost, unmatched_qty, unmatched_revenue, planetary)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (owner_kind, owner_id, transaction_id) DO UPDATE SET
    quantity = excluded.quantity, revenue = excluded.revenue, cost = excluded.cost,
    unmatched_qty = excluded.unmatched_qty, unmatched_revenue = excluded.unmatched_revenue,
    planetary = excluded.planetary;

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
      AND j.at >= sqlc.arg(from_at)::timestamptz AND j.activity NOT IN ('market', 'escrow')
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

-- name: ListTopTradeTypes :many
-- What is actually making money in trading: realized profit per
-- type over the span, best first.
SELECT type_id, SUM(quantity)::bigint AS quantity, SUM(revenue)::float8 AS revenue, SUM(cost)::float8 AS cost
FROM trade_realized
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
  AND day >= sqlc.arg(from_day)::text AND quantity > 0
GROUP BY type_id
ORDER BY SUM(revenue - cost) DESC, type_id
LIMIT sqlc.arg(row_cap)::bigint;

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
-- Open position cost per unit for a character's types: the basis a
-- beaten sell order's margin is measured against.
SELECT type_id, quantity, cost FROM trade_positions
WHERE owner_kind = 'char' AND owner_id = $1 AND type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND quantity > 0;

-- name: GetObservedSalesTaxRate :one
-- A character's effective sales tax rate over a span: transaction
-- tax paid over market sell revenue in the ledger.
SELECT COALESCE((SELECT SUM(-j.amount) FROM ledger_journal j
                 WHERE j.owner_kind = 'char' AND j.owner_id = sqlc.arg(character_id)::bigint
                   AND j.ref_type = 'transaction_tax' AND j.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS tax,
       COALESCE((SELECT SUM(t.quantity * t.unit_price) FROM ledger_txns t
                 WHERE t.owner_kind = 'char' AND t.owner_id = sqlc.arg(character_id)::bigint
                   AND NOT t.is_buy AND t.at >= sqlc.arg(since)::timestamptz), 0)::float8 AS revenue;
