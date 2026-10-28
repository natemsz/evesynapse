-- Admin analytics (097): the recorder's writes and the Analytics
-- section's reads. Comments stay ASCII-only (sqlc parser).

-- ---------------------------------------------------------------------
-- Recorder flush: one transaction per minute, upserts only.
-- ---------------------------------------------------------------------

-- name: UpsertStatsRequest :exec
INSERT INTO stats_requests_minute (minute, route_group, status_class, requests)
VALUES (sqlc.arg(minute)::timestamptz, sqlc.arg(route_group), sqlc.arg(status_class)::smallint, sqlc.arg(requests))
ON CONFLICT (minute, route_group, status_class)
DO UPDATE SET requests = stats_requests_minute.requests + excluded.requests;

-- name: UpsertStatsDuration :exec
INSERT INTO stats_durations_minute (minute, route_group, bucket, hits)
VALUES (sqlc.arg(minute)::timestamptz, sqlc.arg(route_group), sqlc.arg(bucket)::smallint, sqlc.arg(hits))
ON CONFLICT (minute, route_group, bucket)
DO UPDATE SET hits = stats_durations_minute.hits + excluded.hits;

-- name: UpsertStatsESI :exec
INSERT INTO stats_esi_minute (minute, family, calls, errors)
VALUES (sqlc.arg(minute)::timestamptz, sqlc.arg(family), sqlc.arg(calls), sqlc.arg(errors))
ON CONFLICT (minute, family)
DO UPDATE SET calls = stats_esi_minute.calls + excluded.calls,
              errors = stats_esi_minute.errors + excluded.errors;

-- name: UpsertStatsEvent :exec
INSERT INTO stats_events_minute (minute, kind, count)
VALUES (sqlc.arg(minute)::timestamptz, sqlc.arg(kind), sqlc.arg(count))
ON CONFLICT (minute, kind)
DO UPDATE SET count = stats_events_minute.count + excluded.count;

-- name: UpsertStatsHost :exec
INSERT INTO stats_host_minute (minute, load1, load5, load15, mem_used_bytes, mem_total_bytes, app_rss_bytes, goroutines, disk_used_bytes, disk_total_bytes)
VALUES (sqlc.arg(minute)::timestamptz, sqlc.arg(load1), sqlc.arg(load5), sqlc.arg(load15), sqlc.arg(mem_used_bytes), sqlc.arg(mem_total_bytes), sqlc.arg(app_rss_bytes), sqlc.arg(goroutines), sqlc.arg(disk_used_bytes), sqlc.arg(disk_total_bytes))
ON CONFLICT (minute) DO UPDATE SET
    load1 = excluded.load1, load5 = excluded.load5, load15 = excluded.load15,
    mem_used_bytes = excluded.mem_used_bytes, mem_total_bytes = excluded.mem_total_bytes,
    app_rss_bytes = excluded.app_rss_bytes, goroutines = excluded.goroutines,
    disk_used_bytes = excluded.disk_used_bytes, disk_total_bytes = excluded.disk_total_bytes;

-- name: InsertStatsCycle :exec
INSERT INTO stats_cycles (at, took_ms, fetches, budget, deferred, characters, esi_state)
VALUES (sqlc.arg(at)::timestamptz, sqlc.arg(took_ms), sqlc.arg(fetches), sqlc.arg(budget), sqlc.arg(deferred), sqlc.arg(characters), sqlc.arg(esi_state))
ON CONFLICT (at) DO NOTHING;

-- name: InsertStatsCyclePhase :exec
INSERT INTO stats_cycle_phases (cycle_at, phase, took_ms)
VALUES (sqlc.arg(cycle_at)::timestamptz, sqlc.arg(phase), sqlc.arg(took_ms))
ON CONFLICT (cycle_at, phase) DO UPDATE SET took_ms = excluded.took_ms;

-- name: UpsertStatsInventory :exec
INSERT INTO stats_inventory (hour, name, rows_est, size_bytes)
VALUES (sqlc.arg(hour)::timestamptz, sqlc.arg(name), sqlc.arg(rows_est), sqlc.arg(size_bytes))
ON CONFLICT (hour, name) DO UPDATE SET rows_est = excluded.rows_est, size_bytes = excluded.size_bytes;

-- (The inventory snapshot's pg_class reads live in stats_recorder.go
-- as raw SQL: sqlc's schema input has no pg_catalog.)

-- ---------------------------------------------------------------------
-- Rollup and retention. Each rollup runs with its delete in one
-- transaction (the recorder does), so a crash cannot double-count:
-- rolled minutes are gone before the transaction commits.
-- ---------------------------------------------------------------------

-- name: RollupStatsRequests :exec
INSERT INTO stats_requests_hourly (hour, route_group, status_class, requests)
SELECT date_trunc('hour', minute), route_group, status_class, sum(requests)
FROM stats_requests_minute WHERE minute < sqlc.arg(cutoff)::timestamptz
GROUP BY 1, 2, 3
ON CONFLICT (hour, route_group, status_class)
DO UPDATE SET requests = stats_requests_hourly.requests + excluded.requests;

-- name: DeleteStatsRequestsRolled :exec
DELETE FROM stats_requests_minute WHERE minute < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsDurations :exec
INSERT INTO stats_durations_hourly (hour, route_group, bucket, hits)
SELECT date_trunc('hour', minute), route_group, bucket, sum(hits)
FROM stats_durations_minute WHERE minute < sqlc.arg(cutoff)::timestamptz
GROUP BY 1, 2, 3
ON CONFLICT (hour, route_group, bucket)
DO UPDATE SET hits = stats_durations_hourly.hits + excluded.hits;

-- name: DeleteStatsDurationsRolled :exec
DELETE FROM stats_durations_minute WHERE minute < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsESI :exec
INSERT INTO stats_esi_hourly (hour, family, calls, errors)
SELECT date_trunc('hour', minute), family, sum(calls), sum(errors)
FROM stats_esi_minute WHERE minute < sqlc.arg(cutoff)::timestamptz
GROUP BY 1, 2
ON CONFLICT (hour, family)
DO UPDATE SET calls = stats_esi_hourly.calls + excluded.calls,
              errors = stats_esi_hourly.errors + excluded.errors;

-- name: DeleteStatsESIRolled :exec
DELETE FROM stats_esi_minute WHERE minute < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsEvents :exec
INSERT INTO stats_events_hourly (hour, kind, count)
SELECT date_trunc('hour', minute), kind, sum(count)
FROM stats_events_minute WHERE minute < sqlc.arg(cutoff)::timestamptz
GROUP BY 1, 2
ON CONFLICT (hour, kind)
DO UPDATE SET count = stats_events_hourly.count + excluded.count;

-- name: DeleteStatsEventsRolled :exec
DELETE FROM stats_events_minute WHERE minute < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsHost :exec
INSERT INTO stats_host_hourly (hour, samples, load1_sum, load5_sum, load15_sum, mem_used_sum, mem_used_max, mem_total_bytes, app_rss_sum, goroutines_max, disk_used_bytes, disk_total_bytes)
SELECT date_trunc('hour', minute), count(*), sum(load1), sum(load5), sum(load15),
       sum(mem_used_bytes), max(mem_used_bytes), max(mem_total_bytes),
       sum(app_rss_bytes), max(goroutines), max(disk_used_bytes), max(disk_total_bytes)
FROM stats_host_minute WHERE minute < sqlc.arg(cutoff)::timestamptz
GROUP BY 1
ON CONFLICT (hour) DO UPDATE SET
    samples = stats_host_hourly.samples + excluded.samples,
    load1_sum = stats_host_hourly.load1_sum + excluded.load1_sum,
    load5_sum = stats_host_hourly.load5_sum + excluded.load5_sum,
    load15_sum = stats_host_hourly.load15_sum + excluded.load15_sum,
    mem_used_sum = stats_host_hourly.mem_used_sum + excluded.mem_used_sum,
    mem_used_max = GREATEST(stats_host_hourly.mem_used_max, excluded.mem_used_max),
    mem_total_bytes = excluded.mem_total_bytes,
    app_rss_sum = stats_host_hourly.app_rss_sum + excluded.app_rss_sum,
    goroutines_max = GREATEST(stats_host_hourly.goroutines_max, excluded.goroutines_max),
    disk_used_bytes = excluded.disk_used_bytes,
    disk_total_bytes = excluded.disk_total_bytes;

-- name: DeleteStatsHostRolled :exec
DELETE FROM stats_host_minute WHERE minute < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsCycles :exec
INSERT INTO stats_cycles_hourly (hour, cycles, took_sum_ms, took_max_ms, fetches_sum, deferred_sum)
SELECT date_trunc('hour', at), count(*), sum(took_ms), max(took_ms), sum(fetches), sum(deferred)
FROM stats_cycles WHERE at < sqlc.arg(cutoff)::timestamptz
GROUP BY 1
ON CONFLICT (hour) DO UPDATE SET
    cycles = stats_cycles_hourly.cycles + excluded.cycles,
    took_sum_ms = stats_cycles_hourly.took_sum_ms + excluded.took_sum_ms,
    took_max_ms = GREATEST(stats_cycles_hourly.took_max_ms, excluded.took_max_ms),
    fetches_sum = stats_cycles_hourly.fetches_sum + excluded.fetches_sum,
    deferred_sum = stats_cycles_hourly.deferred_sum + excluded.deferred_sum;

-- name: DeleteStatsCyclesRolled :exec
DELETE FROM stats_cycles WHERE at < sqlc.arg(cutoff)::timestamptz;

-- name: RollupStatsCyclePhases :exec
INSERT INTO stats_cycle_phases_hourly (hour, phase, took_sum_ms, samples)
SELECT date_trunc('hour', cycle_at), phase, sum(took_ms), count(*)
FROM stats_cycle_phases WHERE cycle_at < sqlc.arg(cutoff)::timestamptz
GROUP BY 1, 2
ON CONFLICT (hour, phase) DO UPDATE SET
    took_sum_ms = stats_cycle_phases_hourly.took_sum_ms + excluded.took_sum_ms,
    samples = stats_cycle_phases_hourly.samples + excluded.samples;

-- name: DeleteStatsCyclePhasesRolled :exec
DELETE FROM stats_cycle_phases WHERE cycle_at < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsRequestsHourly :exec
DELETE FROM stats_requests_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsDurationsHourly :exec
DELETE FROM stats_durations_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsESIHourly :exec
DELETE FROM stats_esi_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsEventsHourly :exec
DELETE FROM stats_events_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsHostHourly :exec
DELETE FROM stats_host_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsCyclesHourly :exec
DELETE FROM stats_cycles_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsCyclePhasesHourly :exec
DELETE FROM stats_cycle_phases_hourly WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- name: PruneStatsInventory :exec
DELETE FROM stats_inventory WHERE hour < sqlc.arg(cutoff)::timestamptz;

-- ---------------------------------------------------------------------
-- Analytics reads: minute tables for the 24h and 7d ranges, hourly
-- rollups for 30d. Bucket steps arrive in seconds; bucketing is
-- epoch math so one query serves every range.
-- ---------------------------------------------------------------------

-- name: StatsTrafficMinutes :many
SELECT (to_timestamp(floor(extract(epoch FROM minute) / sqlc.arg(step_seconds)::float8) * sqlc.arg(step_seconds)::float8))::timestamptz AS bucket,
       status_class, sum(requests)::bigint AS requests
FROM stats_requests_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1, 2 ORDER BY 1, 2;

-- name: StatsTrafficHours :many
SELECT (to_timestamp(floor(extract(epoch FROM hour) / sqlc.arg(step_seconds)::float8) * sqlc.arg(step_seconds)::float8))::timestamptz AS bucket,
       status_class, sum(requests)::bigint AS requests
FROM stats_requests_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1, 2 ORDER BY 1, 2;

-- name: StatsTopGroupsMinutes :many
SELECT route_group, sum(requests)::bigint AS requests
FROM stats_requests_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 2 DESC LIMIT sqlc.arg(lim);

-- name: StatsTopGroupsHours :many
SELECT route_group, sum(requests)::bigint AS requests
FROM stats_requests_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 2 DESC LIMIT sqlc.arg(lim);

-- name: StatsDurationsForGroupsMinutes :many
SELECT route_group, bucket, sum(hits)::bigint AS hits
FROM stats_durations_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
  AND route_group = ANY(sqlc.arg(groups)::text[])
GROUP BY 1, 2;

-- name: StatsDurationsForGroupsHours :many
SELECT route_group, bucket, sum(hits)::bigint AS hits
FROM stats_durations_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
  AND route_group = ANY(sqlc.arg(groups)::text[])
GROUP BY 1, 2;

-- name: StatsDurationsAllMinutes :many
SELECT bucket, sum(hits)::bigint AS hits
FROM stats_durations_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsDurationsAllHours :many
SELECT bucket, sum(hits)::bigint AS hits
FROM stats_durations_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsESIWindowMinutes :many
SELECT family, sum(calls)::bigint AS calls, sum(errors)::bigint AS errors
FROM stats_esi_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 2 DESC LIMIT sqlc.arg(lim);

-- name: StatsESIWindowHours :many
SELECT family, sum(calls)::bigint AS calls, sum(errors)::bigint AS errors
FROM stats_esi_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 2 DESC LIMIT sqlc.arg(lim);

-- name: StatsEventsWindowMinutes :many
SELECT kind, sum(count)::bigint AS count
FROM stats_events_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsEventsWindowHours :many
SELECT kind, sum(count)::bigint AS count
FROM stats_events_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsCycleSeries :many
SELECT (to_timestamp(floor(extract(epoch FROM at) / sqlc.arg(step_seconds)::float8) * sqlc.arg(step_seconds)::float8))::timestamptz AS bucket,
       avg(took_ms)::float8 AS avg_ms, count(*)::bigint AS cycles
FROM stats_cycles
WHERE at >= sqlc.arg(from_ts)::timestamptz AND at < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: StatsCycleSeriesHours :many
SELECT hour AS bucket,
       (sum(took_sum_ms) / NULLIF(sum(cycles), 0))::float8 AS avg_ms,
       sum(cycles)::bigint AS cycles
FROM stats_cycles_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: StatsCycleSummary :one
SELECT count(*)::bigint AS cycles,
       coalesce(avg(took_ms), 0)::float8 AS avg_ms,
       coalesce(max(took_ms), 0)::bigint AS max_ms,
       coalesce((percentile_cont(0.95) WITHIN GROUP (ORDER BY took_ms))::float8, 0::float8) AS p95_ms,
       coalesce(sum(fetches), 0)::bigint AS fetches,
       coalesce(sum(deferred), 0)::bigint AS deferred
FROM stats_cycles
WHERE at >= sqlc.arg(from_ts)::timestamptz AND at < sqlc.arg(to_ts)::timestamptz;

-- name: StatsCycleSummaryHours :one
SELECT coalesce(sum(cycles), 0)::bigint AS cycles,
       coalesce((sum(took_sum_ms) / NULLIF(sum(cycles), 0))::float8, 0::float8) AS avg_ms,
       coalesce(max(took_max_ms), 0)::bigint AS max_ms,
       coalesce(sum(fetches_sum), 0)::bigint AS fetches,
       coalesce(sum(deferred_sum), 0)::bigint AS deferred
FROM stats_cycles_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz;

-- name: StatsCyclePhasesWindow :many
SELECT phase, sum(took_ms)::bigint AS took_sum_ms, count(*)::bigint AS samples
FROM stats_cycle_phases
WHERE cycle_at >= sqlc.arg(from_ts)::timestamptz AND cycle_at < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsCyclePhasesHours :many
SELECT phase, sum(took_sum_ms)::bigint AS took_sum_ms, sum(samples)::bigint AS samples
FROM stats_cycle_phases_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1;

-- name: StatsHostSeries :many
SELECT (to_timestamp(floor(extract(epoch FROM minute) / sqlc.arg(step_seconds)::float8) * sqlc.arg(step_seconds)::float8))::timestamptz AS bucket,
       avg(load1)::float8 AS load1, avg(mem_used_bytes)::float8 AS mem_used_bytes,
       max(mem_total_bytes)::bigint AS mem_total_bytes
FROM stats_host_minute
WHERE minute >= sqlc.arg(from_ts)::timestamptz AND minute < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: StatsHostSeriesHours :many
SELECT hour AS bucket,
       (sum(load1_sum) / NULLIF(sum(samples), 0))::float8 AS load1,
       (sum(mem_used_sum) / NULLIF(sum(samples), 0))::float8 AS mem_used_bytes,
       max(mem_total_bytes)::bigint AS mem_total_bytes
FROM stats_host_hourly
WHERE hour >= sqlc.arg(from_ts)::timestamptz AND hour < sqlc.arg(to_ts)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: StatsHostLatest :one
SELECT minute, load1, load5, load15, mem_used_bytes, mem_total_bytes, app_rss_bytes, goroutines, disk_used_bytes, disk_total_bytes
FROM stats_host_minute
ORDER BY minute DESC LIMIT 1;

-- name: StatsInventoryLatest :many
SELECT name, rows_est, size_bytes
FROM stats_inventory
WHERE hour = (SELECT max(hour) FROM stats_inventory)
ORDER BY size_bytes DESC LIMIT sqlc.arg(lim);

-- ---------------------------------------------------------------------
-- Growth, activity and freshness: single aggregate reads over the
-- owning tables, each with an indexed window predicate.
-- ---------------------------------------------------------------------

-- name: StatsSignupsByDay :many
SELECT date_trunc('day', created_at)::timestamptz AS day, count(*)::bigint AS signups
FROM users
WHERE created_at >= sqlc.arg(from_ts)::timestamptz
GROUP BY 1 ORDER BY 1;

-- name: StatsActiveSessions :one
SELECT count(*)::bigint AS active FROM sessions WHERE expiry > now();

-- name: CountIntelReportsWindow :one
SELECT count(*)::bigint AS n FROM intel_reports
WHERE created_at >= sqlc.arg(from_ts)::timestamptz AND created_at < sqlc.arg(to_ts)::timestamptz;

-- name: CountAppraisalsWindow :one
SELECT count(*)::bigint AS n FROM user_appraisals
WHERE created_at >= sqlc.arg(from_ts)::timestamptz AND created_at < sqlc.arg(to_ts)::timestamptz;

-- name: CountFitsSavedWindow :one
SELECT count(*)::bigint AS n FROM local_fittings
WHERE created_at >= sqlc.arg(from_ts)::timestamptz AND created_at < sqlc.arg(to_ts)::timestamptz;

-- name: CountCharactersLinkedWindow :one
SELECT count(*)::bigint AS n FROM characters
WHERE created_at >= sqlc.arg(from_ts)::timestamptz AND created_at < sqlc.arg(to_ts)::timestamptz;

-- name: StatsFetchStatesByKinds :many
SELECT kind, attempted_at FROM market_fetch_state
WHERE kind = ANY(sqlc.arg(kinds)::text[]);

-- name: StatsMarketHistoryLatest :one
SELECT max(attempted_at) AS latest FROM market_fetch_state WHERE starts_with(kind, 'history_');

-- name: StatsPubContractSweeps :many
SELECT region_id, updated_at FROM pub_contract_sweep_state
WHERE region_id = ANY(sqlc.arg(regions)::bigint[]);

-- name: StatsPubContractsDeparted :one
SELECT count(*) AS departed FROM pub_contracts WHERE departed_at IS NOT NULL;

-- name: StatsTrueValuesLatest :one
SELECT max(computed_at) AS latest FROM true_values;

-- name: StatsIntelLatest :one
SELECT max(created_at) AS latest FROM intel_reports;

-- name: StatsSwarmLastApplied :one
SELECT max(applied_at) AS latest FROM swarm_applied;
