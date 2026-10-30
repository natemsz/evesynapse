-- The Profit page's charts: each series summed in SQL into day or
-- week buckets over the span, at most about a year of buckets.

-- name: ListProfitBuckets :many
-- Income and costs per bucket and activity.
SELECT to_char(date_trunc(sqlc.arg(unit)::text, day::date), 'YYYY-MM-DD') AS bucket, activity,
       SUM(income)::float8 AS income, SUM(costs)::float8 AS costs
FROM profit_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND day >= sqlc.arg(from_day)::text
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: ListProfitOwnerBuckets :many
-- Attributed net per owner and bucket, for the per-character lines.
SELECT owner_id, to_char(date_trunc(sqlc.arg(unit)::text, day::date), 'YYYY-MM-DD') AS bucket,
       SUM(income - costs)::float8 AS net
FROM profit_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND day >= sqlc.arg(from_day)::text
  AND activity <> 'unattributed'
GROUP BY 1, 2
ORDER BY 1, 2;

-- name: ListProfitFeeBuckets :many
-- Fees per bucket by kind, and market sell revenue to measure them
-- against.
SELECT f.bucket,
       COALESCE(SUM(f.sales_tax), 0)::float8 AS sales_tax, COALESCE(SUM(f.broker), 0)::float8 AS broker,
       COALESCE(SUM(f.industry), 0)::float8 AS industry, COALESCE(SUM(f.lp_store), 0)::float8 AS lp_store,
       COALESCE(SUM(f.revenue), 0)::float8 AS revenue
FROM (
    SELECT to_char(date_trunc(sqlc.arg(unit)::text, j.at AT TIME ZONE 'UTC'), 'YYYY-MM-DD') AS bucket,
           CASE WHEN j.ref_type = 'transaction_tax' THEN -j.amount ELSE 0 END AS sales_tax,
           CASE WHEN j.ref_type = 'brokers_fee' THEN -j.amount ELSE 0 END AS broker,
           CASE WHEN j.activity = 'industry' AND j.amount < 0 THEN -j.amount ELSE 0 END AS industry,
           CASE WHEN j.ref_type = 'lp_store' THEN -j.amount ELSE 0 END AS lp_store,
           0::float8 AS revenue
    FROM ledger_journal j
    WHERE j.owner_kind = sqlc.arg(owner_kind) AND j.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
      AND j.at >= sqlc.arg(from_at)::timestamptz
      AND (j.ref_type IN ('transaction_tax', 'brokers_fee', 'lp_store') OR j.activity = 'industry')
    UNION ALL
    SELECT to_char(date_trunc(sqlc.arg(unit)::text, t.at AT TIME ZONE 'UTC'), 'YYYY-MM-DD'), 0, 0, 0, 0, t.quantity * t.unit_price
    FROM ledger_txns t
    WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = ANY(sqlc.arg(owner_ids)::bigint[])
      AND t.at >= sqlc.arg(from_at)::timestamptz AND NOT t.is_buy
) f
GROUP BY f.bucket
ORDER BY f.bucket;

-- name: ListTradeCoverageDaily :many
-- Trading's covered figures per day, for the rolling margin and the
-- uncovered days every chart marks.
SELECT day, SUM(units)::bigint AS units, SUM(cov_units)::bigint AS cov_units,
       SUM(cov_revenue)::float8 AS cov_revenue, SUM(cov_cost)::float8 AS cov_cost,
       SUM(cov_tax)::float8 AS cov_tax, SUM(broker)::float8 AS broker
FROM trade_item_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND day >= sqlc.arg(from_day)::text
GROUP BY day
ORDER BY day;

-- name: ListTradeCOGSBuckets :many
-- Trading's sales per bucket: all that sold, the covered part (units
-- with a known cost and a matched tax line), and what those goods cost.
SELECT to_char(date_trunc(sqlc.arg(unit)::text, day::date), 'YYYY-MM-DD') AS bucket,
       SUM(revenue)::float8 AS revenue, SUM(cov_revenue)::float8 AS cov_revenue, SUM(cov_cost)::float8 AS cov_cost
FROM trade_item_daily
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = ANY(sqlc.arg(owner_ids)::bigint[]) AND day >= sqlc.arg(from_day)::text
GROUP BY 1
ORDER BY 1;
