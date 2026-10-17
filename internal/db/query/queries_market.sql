-- name: DeleteGuidePrices :exec
DELETE FROM guide_prices;
-- name: UpsertGuidePrice :exec
INSERT INTO guide_prices (type_id, adjusted_price, average_price)
VALUES ($1, $2, $3)
ON CONFLICT (type_id) DO UPDATE SET
    adjusted_price = excluded.adjusted_price,
    average_price  = excluded.average_price;
-- name: ListGuidePrices :many
SELECT type_id, adjusted_price, average_price FROM guide_prices
ORDER BY type_id;
-- name: GetGuidePricesMeta :one
SELECT id, fetched_at, cached_until FROM guide_prices_meta
WHERE id = 1;
-- name: UpsertGuidePricesMeta :exec
INSERT INTO guide_prices_meta (id, fetched_at, cached_until)
VALUES (1, $1, $2)
ON CONFLICT (id) DO UPDATE SET
    fetched_at   = excluded.fetched_at,
    cached_until = excluded.cached_until;


-- ---------------------------------------------------------------------
-- Guide-price wants (schema 027): the durable note a
-- kill view leaves when it has no prices to value with. One
-- singleton row -- the guide is global, so one want covers every
-- viewer -- noted at render time, answered by the worker's
-- urgent drain with a guide refresh when ESI's window allows.
-- ---------------------------------------------------------------------
-- name: NoteGuidePriceWant :exec
INSERT INTO guide_price_wants (id, wanted_at)
VALUES (1, $1)
ON CONFLICT (id) DO UPDATE SET
    wanted_at = excluded.wanted_at;
-- name: GetGuidePriceWant :one
SELECT id, wanted_at FROM guide_price_wants
WHERE id = 1;
-- name: ClearGuidePriceWant :exec
DELETE FROM guide_price_wants
WHERE id = 1;

-- ---------------------------------------------------------------------
-- Industry build planner reads. Bulk import
-- inserts stay hand-rolled in the SDE importer alongside the other
-- sde_* tables; only reads live here.
-- ---------------------------------------------------------------------
-- name: UpsertMarketHistory :exec
INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (region_id, type_id, date) DO UPDATE SET
    average     = excluded.average,
    highest     = excluded.highest,
    lowest      = excluded.lowest,
    volume      = excluded.volume,
    order_count = excluded.order_count;
-- name: ListMarketHistory :many
-- The history window is a row count, not a calendar span: the
-- chart and change math read the newest N rows of recorded
-- trades, newest first, so a sparse item's stored trades are
-- never hidden by an arbitrary date cutoff.
SELECT region_id, type_id, date, average, highest, lowest, volume, order_count
FROM market_history
WHERE region_id = $1 AND type_id = $2
ORDER BY date DESC
LIMIT sqlc.arg(row_limit)::bigint;
-- name: UpsertMarketHistoryWant :exec
INSERT INTO market_history_wants (region_id, type_id, last_requested_at, priority)
VALUES ($1, $2, $3, sqlc.arg(priority))
ON CONFLICT (region_id, type_id) DO UPDATE SET
    last_requested_at = excluded.last_requested_at,
    priority = GREATEST(market_history_wants.priority, excluded.priority);
-- name: ListMarketHistoryWants :many
-- Typing rows spend their slice highest learned rate first (see
-- ListPilotDrains).
SELECT region_id, type_id, last_requested_at
FROM (
    SELECT region_id, type_id, last_requested_at, priority,
        ROW_NUMBER() OVER (
            PARTITION BY (priority = sqlc.arg(typing_priority)::bigint)
            ORDER BY type_rate DESC NULLS LAST, last_requested_at DESC
        ) AS typing_n
    FROM (
        SELECT hw.region_id, hw.type_id, hw.last_requested_at, hw.priority,
            ((COUNT(tg.hit_at) + sqlc.arg(typing_assumed)::float8 * sqlc.arg(typing_weight)::float8)
                / (COUNT(tg.entity_id) + sqlc.arg(typing_weight)::float8)) AS type_rate
        FROM market_history_wants hw
        LEFT JOIN typing_guesses tg
            ON tg.kind = 'market_history' AND tg.entity_id = hw.type_id AND tg.region_id = hw.region_id
            AND tg.noted_at >= sqlc.arg(typing_since)::timestamptz
        WHERE hw.last_requested_at >= $1
        GROUP BY hw.region_id, hw.type_id, hw.last_requested_at, hw.priority
    ) AS rated
) AS wants
WHERE priority != sqlc.arg(typing_priority)::bigint OR typing_n <= sqlc.arg(typing_limit)::bigint
ORDER BY priority DESC,
    CASE WHEN priority = sqlc.arg(typing_priority)::bigint THEN typing_n END ASC NULLS LAST,
    last_requested_at DESC;
-- name: GetMarketFetchState :one
SELECT kind, state, detail, attempted_at
FROM market_fetch_state
WHERE kind = $1;
-- name: UpsertMarketFetchState :exec
INSERT INTO market_fetch_state (kind, state, detail, attempted_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (kind) DO UPDATE SET
    state        = excluded.state,
    detail       = excluded.detail,
    attempted_at = excluded.attempted_at;
-- name: ListWatchlistByUser :many
SELECT user_id, type_id, region_id, threshold_pct, created_at
FROM market_watchlist
WHERE user_id = $1
ORDER BY type_id, region_id;
-- name: ListAllWatchlistEntries :many
SELECT user_id, type_id, region_id, threshold_pct, created_at
FROM market_watchlist
ORDER BY type_id, region_id;
-- name: GetWatchlistEntry :one
SELECT user_id, type_id, region_id, threshold_pct, created_at
FROM market_watchlist
WHERE user_id = $1 AND type_id = $2 AND region_id = $3;
-- name: UpsertWatchlistEntry :exec
INSERT INTO market_watchlist (user_id, type_id, region_id, threshold_pct, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, type_id, region_id) DO UPDATE SET
    threshold_pct = excluded.threshold_pct;
-- name: DeleteWatchlistEntry :exec
DELETE FROM market_watchlist
WHERE user_id = $1 AND type_id = $2 AND region_id = $3;
-- name: UpsertOrderHealth :exec
INSERT INTO order_health (character_id, order_id, type_id, region_id, location_id, my_price, station_best, region_best, status, computed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (character_id, order_id) DO UPDATE SET
    type_id      = excluded.type_id,
    region_id    = excluded.region_id,
    location_id  = excluded.location_id,
    my_price     = excluded.my_price,
    station_best = excluded.station_best,
    region_best  = excluded.region_best,
    status       = excluded.status,
    computed_at  = excluded.computed_at;
-- name: DeleteOrderHealthForCharacter :exec
DELETE FROM order_health
WHERE character_id = $1;
-- name: DeleteOrderHealthEntry :exec
DELETE FROM order_health
WHERE character_id = $1 AND order_id = $2;
-- name: ListOrderHealthByCharacter :many
SELECT character_id, order_id, type_id, region_id, location_id, my_price, station_best, region_best, status, computed_at
FROM order_health
WHERE character_id = $1
ORDER BY type_id, order_id;
-- name: ListOrderHealthByUser :many
SELECT oh.character_id, oh.order_id, oh.type_id, oh.region_id, oh.location_id, oh.my_price, oh.station_best, oh.region_best, oh.status, oh.computed_at
FROM order_health oh
JOIN characters c ON c.character_id = oh.character_id
WHERE c.user_id = $1
ORDER BY c.name, oh.type_id, oh.order_id;

-- ---------------------------------------------------------------------
-- Player structure names (schema 014): queued when a worker-computed
-- view or the market book view meets an unresolved structure id,
-- resolved in the background via any linked character holding
-- esi-universe.read_structures.v1 (the lookup endpoint is
-- authenticated-only). 'resolved' rows re-check after 30 days
-- (structures can be renamed); 'missing' rows (403/404: private or
-- gone) re-check after 24 hours; 'pending' rows are always due.
-- A 'resolved' row with no structure_context row is also due, so
-- a name the character lookup resolved picks up the owner/system/
-- type facts that lookup carries without waiting out the rename
-- window.
-- The worker tries every eligible linked character before a
-- negative answer is cached, and 'source' (schema 023) records the
-- name's provenance so ESI truth outranks any lower-trust tier.
-- ---------------------------------------------------------------------
-- name: GetStructureName :one
SELECT structure_id, name, state, resolved_at, source
FROM structure_names
WHERE structure_id = $1;
-- name: UpsertStructureSeen :exec
INSERT INTO structure_names (structure_id)
VALUES ($1)
ON CONFLICT DO NOTHING;
-- name: SetStructureName :exec
INSERT INTO structure_names (structure_id, name, state, resolved_at, source)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (structure_id) DO UPDATE SET
    name        = excluded.name,
    state       = excluded.state,
    resolved_at = excluded.resolved_at,
    source      = excluded.source;
-- name: ListStructureResolutions :many
SELECT structure_id
FROM structure_names
WHERE state = 'pending'
   OR (state = 'resolved' AND (resolved_at IS NULL OR resolved_at < sqlc.arg(resolved_cutoff)::timestamptz))
   OR (state = 'resolved' AND NOT EXISTS (SELECT 1 FROM structure_context WHERE structure_context.structure_id = structure_names.structure_id))
   OR (state = 'missing' AND (resolved_at IS NULL OR resolved_at < sqlc.arg(missing_cutoff)::timestamptz))
ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, structure_id
LIMIT sqlc.arg(resolution_limit)::bigint;

-- ---------------------------------------------------------------------
-- Structure context (schema 028): owner/system/type facts the
-- corporation structure snapshots report, persisted as the
-- snapshots are processed so the structure page renders from one
-- small row. A successful per-structure lookup writes the same
-- facts from its own payload, so structures no snapshot covers
-- still gain context. Both writers are ESI truth; the fresher
-- write wins. Names are NOT stored here (they live in
-- structure_names under provenance rules).
-- ---------------------------------------------------------------------
-- name: GetStructureContext :one
SELECT structure_id, owner_corporation_id, system_id, type_id, updated_at
FROM structure_context
WHERE structure_id = $1;
-- name: SetStructureContext :exec
INSERT INTO structure_context (structure_id, owner_corporation_id, system_id, type_id, updated_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (structure_id) DO UPDATE SET
    owner_corporation_id = excluded.owner_corporation_id,
    system_id            = excluded.system_id,
    type_id              = excluded.type_id,
    updated_at           = excluded.updated_at;

-- ---------------------------------------------------------------------
-- Planet names (schema 025): queued when a PI surface meets an
-- unresolved planet id, resolved in the background through the
-- public GET /universe/planets/{id}/ (no token). 'resolved' rows
-- re-check after a long window (planet names never change);
-- 'missing' rows (404: not a planet) re-check after days;
-- 'pending' rows are always due.
-- ---------------------------------------------------------------------
-- name: GetPlanetName :one
SELECT planet_id, name, state, resolved_at
FROM planet_names
WHERE planet_id = $1;
-- name: UpsertPlanetSeen :exec
INSERT INTO planet_names (planet_id)
VALUES ($1)
ON CONFLICT DO NOTHING;
-- name: SetPlanetName :exec
INSERT INTO planet_names (planet_id, name, state, resolved_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (planet_id) DO UPDATE SET
    name        = excluded.name,
    state       = excluded.state,
    resolved_at = excluded.resolved_at;
-- name: ListPlanetResolutions :many
SELECT planet_id
FROM planet_names
WHERE state = 'pending'
   OR (state = 'resolved' AND (resolved_at IS NULL OR resolved_at < sqlc.arg(resolved_cutoff)::timestamptz))
   OR (state = 'missing' AND (resolved_at IS NULL OR resolved_at < sqlc.arg(missing_cutoff)::timestamptz))
ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, planet_id
LIMIT sqlc.arg(resolution_limit)::bigint;

-- ---------------------------------------------------------------------
-- Public pilot records (schema 015): the queue behind /pilot/.
-- Pending rows are always due; ready rows re-check once their
-- fetched_at passes the stale cutoff; missing rows (ESI 404)
-- settle for good. The item details page enqueues type
-- descriptions the same way: a type_details row with no
-- fetched_at is a want.
-- ---------------------------------------------------------------------
-- name: GetPilotRecord :one
SELECT character_id, payload, state, fetched_at
FROM pilot_records
WHERE character_id = $1;
-- A want's priority is how close the record is to somebody (see
-- wantViewed and its fellows in worker_pagewants.go), and noted_at is
-- when it was last asked for. A want that is asked for again keeps
-- the closer of its priorities and takes the later time.
-- name: UpsertPilotWant :exec
INSERT INTO pilot_records (character_id, priority, noted_at)
VALUES ($1, sqlc.arg(priority), sqlc.arg(noted_at))
ON CONFLICT (character_id) DO UPDATE SET
    priority = GREATEST(pilot_records.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: InsertPilotOrbitWant :exec
INSERT INTO pilot_records (character_id, priority, noted_at)
VALUES ($1, 1, sqlc.arg(noted_at))
ON CONFLICT DO NOTHING;
-- name: ListPilotRecordIDs :many
SELECT character_id
FROM pilot_records;
-- name: SetPilotRecord :exec
INSERT INTO pilot_records (character_id, payload, state, fetched_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (character_id) DO UPDATE SET
    payload    = excluded.payload,
    state      = excluded.state,
    fetched_at = excluded.fetched_at;
-- name: ListPilotDrains :many
-- Typing rows spend their slice highest learned rate first: the
-- share of recent guesses for the pilot that were opened, blended
-- with the assumed rate. Recency breaks ties; unknown pilots read
-- at the assumed rate, so a cold start drains newest first.
SELECT character_id
FROM (
    SELECT character_id, priority, noted_at, fetched_at, state,
        ROW_NUMBER() OVER (
            PARTITION BY (priority = sqlc.arg(typing_priority)::bigint)
            ORDER BY type_rate DESC NULLS LAST, noted_at DESC NULLS LAST
        ) AS typing_n
    FROM (
        SELECT pr.character_id, pr.priority, pr.noted_at, pr.fetched_at, pr.state,
            ((COUNT(tg.hit_at) + sqlc.arg(typing_assumed)::float8 * sqlc.arg(typing_weight)::float8)
                / (COUNT(tg.entity_id) + sqlc.arg(typing_weight)::float8)) AS type_rate
        FROM pilot_records pr
        LEFT JOIN typing_guesses tg
            ON tg.kind = 'pilot' AND tg.entity_id = pr.character_id
            AND tg.noted_at >= sqlc.arg(typing_since)::timestamptz
        WHERE pr.state = 'pending'
           OR (pr.state = 'ready' AND (pr.fetched_at IS NULL OR pr.fetched_at < sqlc.arg(stale_cutoff)::timestamptz))
        GROUP BY pr.character_id, pr.priority, pr.noted_at, pr.fetched_at, pr.state
    ) AS rated
) AS drains
WHERE priority != sqlc.arg(typing_priority)::bigint OR typing_n <= sqlc.arg(typing_limit)::bigint
ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, priority DESC,
    CASE WHEN priority = sqlc.arg(typing_priority)::bigint THEN typing_n END ASC NULLS LAST,
    noted_at DESC NULLS LAST, fetched_at NULLS FIRST
LIMIT sqlc.arg(drain_limit)::bigint;

-- ---------------------------------------------------------------------
-- Public corporation & alliance records (schema 026): the queues
-- behind /corporation/ and /alliance/. Same posture as pilot
-- records: pending rows are always due, ready rows re-check past
-- the stale cutoff, missing rows settle for good.
-- ---------------------------------------------------------------------
-- name: GetCorporationRecord :one
SELECT corporation_id, payload, state, fetched_at, priority
FROM corporation_records
WHERE corporation_id = $1;
-- name: UpsertCorporationWant :exec
INSERT INTO corporation_records (corporation_id, priority, noted_at)
VALUES ($1, sqlc.arg(priority), sqlc.arg(noted_at))
ON CONFLICT (corporation_id) DO UPDATE SET
    priority = GREATEST(corporation_records.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: UpsertCorporationWants :exec
-- Many corporation wants in one statement: a journal harvest
-- meets the same parties over and over, and noting each in its
-- own round trip is the N+1 the flush in name_harvest.go avoids.
INSERT INTO corporation_records (corporation_id, priority, noted_at)
SELECT unnest(sqlc.arg(corporation_ids)::bigint[]), sqlc.arg(priority)::bigint, sqlc.arg(noted_at)::timestamptz
ON CONFLICT (corporation_id) DO UPDATE SET
    priority = GREATEST(corporation_records.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: SetCorporationRecord :exec
INSERT INTO corporation_records (corporation_id, payload, state, fetched_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (corporation_id) DO UPDATE SET
    payload    = excluded.payload,
    state      = excluded.state,
    fetched_at = excluded.fetched_at;
-- name: ListCorporationDrains :many
-- Typing rows spend their slice highest learned rate first (see
-- ListPilotDrains).
SELECT corporation_id
FROM (
    SELECT corporation_id, priority, noted_at, fetched_at, state,
        ROW_NUMBER() OVER (
            PARTITION BY (priority = sqlc.arg(typing_priority)::bigint)
            ORDER BY type_rate DESC NULLS LAST, noted_at DESC NULLS LAST
        ) AS typing_n
    FROM (
        SELECT cr.corporation_id, cr.priority, cr.noted_at, cr.fetched_at, cr.state,
            ((COUNT(tg.hit_at) + sqlc.arg(typing_assumed)::float8 * sqlc.arg(typing_weight)::float8)
                / (COUNT(tg.entity_id) + sqlc.arg(typing_weight)::float8)) AS type_rate
        FROM corporation_records cr
        LEFT JOIN typing_guesses tg
            ON tg.kind = 'corporation' AND tg.entity_id = cr.corporation_id
            AND tg.noted_at >= sqlc.arg(typing_since)::timestamptz
        WHERE cr.state = 'pending'
           OR (cr.state = 'ready' AND (cr.fetched_at IS NULL OR cr.fetched_at < sqlc.arg(stale_cutoff)::timestamptz))
        GROUP BY cr.corporation_id, cr.priority, cr.noted_at, cr.fetched_at, cr.state
    ) AS rated
) AS drains
WHERE priority != sqlc.arg(typing_priority)::bigint OR typing_n <= sqlc.arg(typing_limit)::bigint
ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, priority DESC,
    CASE WHEN priority = sqlc.arg(typing_priority)::bigint THEN typing_n END ASC NULLS LAST,
    noted_at DESC NULLS LAST, fetched_at NULLS FIRST
LIMIT sqlc.arg(drain_limit)::bigint;
-- name: GetAllianceRecord :one
SELECT alliance_id, payload, state, fetched_at, priority
FROM alliance_records
WHERE alliance_id = $1;
-- name: UpsertAllianceWant :exec
INSERT INTO alliance_records (alliance_id, priority, noted_at)
VALUES ($1, sqlc.arg(priority), sqlc.arg(noted_at))
ON CONFLICT (alliance_id) DO UPDATE SET
    priority = GREATEST(alliance_records.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: UpsertAllianceWants :exec
-- Many alliance wants in one statement: the alliance half of
-- UpsertCorporationWants (see above).
INSERT INTO alliance_records (alliance_id, priority, noted_at)
SELECT unnest(sqlc.arg(alliance_ids)::bigint[]), sqlc.arg(priority)::bigint, sqlc.arg(noted_at)::timestamptz
ON CONFLICT (alliance_id) DO UPDATE SET
    priority = GREATEST(alliance_records.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: SetAllianceRecord :exec
INSERT INTO alliance_records (alliance_id, payload, state, fetched_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (alliance_id) DO UPDATE SET
    payload    = excluded.payload,
    state      = excluded.state,
    fetched_at = excluded.fetched_at;
-- name: ListAllianceDrains :many
-- Typing rows spend their slice highest learned rate first (see
-- ListPilotDrains).
SELECT alliance_id
FROM (
    SELECT alliance_id, priority, noted_at, fetched_at, state,
        ROW_NUMBER() OVER (
            PARTITION BY (priority = sqlc.arg(typing_priority)::bigint)
            ORDER BY type_rate DESC NULLS LAST, noted_at DESC NULLS LAST
        ) AS typing_n
    FROM (
        SELECT ar.alliance_id, ar.priority, ar.noted_at, ar.fetched_at, ar.state,
            ((COUNT(tg.hit_at) + sqlc.arg(typing_assumed)::float8 * sqlc.arg(typing_weight)::float8)
                / (COUNT(tg.entity_id) + sqlc.arg(typing_weight)::float8)) AS type_rate
        FROM alliance_records ar
        LEFT JOIN typing_guesses tg
            ON tg.kind = 'alliance' AND tg.entity_id = ar.alliance_id
            AND tg.noted_at >= sqlc.arg(typing_since)::timestamptz
        WHERE ar.state = 'pending'
           OR (ar.state = 'ready' AND (ar.fetched_at IS NULL OR ar.fetched_at < sqlc.arg(stale_cutoff)::timestamptz))
        GROUP BY ar.alliance_id, ar.priority, ar.noted_at, ar.fetched_at, ar.state
    ) AS rated
) AS drains
WHERE priority != sqlc.arg(typing_priority)::bigint OR typing_n <= sqlc.arg(typing_limit)::bigint
ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, priority DESC,
    CASE WHEN priority = sqlc.arg(typing_priority)::bigint THEN typing_n END ASC NULLS LAST,
    noted_at DESC NULLS LAST, fetched_at NULLS FIRST
LIMIT sqlc.arg(drain_limit)::bigint;

-- Pilot name-resolution wants (schema 022): a topbar search for
-- a pilot name no local tier knows notes the name once; the
-- worker resolves due rows through ESI's public name lookup.
-- 'missing' and 'ready' are settled states; 'error' rows wait
-- for next_try_at so a failing lookup is not re-asked per search.
-- name: GetPilotNameWant :one
SELECT normalized_name, display_name, state, character_id, requested_at, resolved_at, next_try_at, attempts
FROM pilot_name_wants
WHERE normalized_name = $1;
-- name: UpsertPilotNameWant :exec
INSERT INTO pilot_name_wants (normalized_name, display_name, state, requested_at)
VALUES ($1, $2, 'pending', $3)
ON CONFLICT DO NOTHING;
-- name: ListDuePilotNameWants :many
SELECT normalized_name, display_name, state, character_id, requested_at, resolved_at, next_try_at, attempts
FROM pilot_name_wants
WHERE (state = 'pending' OR state = 'error')
  AND (next_try_at IS NULL OR next_try_at <= sqlc.arg(now)::timestamptz)
ORDER BY requested_at
LIMIT sqlc.arg(lim)::bigint;
-- name: SetPilotNameWantReady :exec
UPDATE pilot_name_wants
SET state = 'ready', character_id = $1, resolved_at = $2, next_try_at = NULL
WHERE normalized_name = $3;
-- name: SetPilotNameWantMissing :exec
UPDATE pilot_name_wants
SET state = 'missing', resolved_at = $1, next_try_at = NULL
WHERE normalized_name = $2;
-- name: SetPilotNameWantError :exec
UPDATE pilot_name_wants
SET state = 'error', attempts = attempts + 1, resolved_at = $1, next_try_at = $2
WHERE normalized_name = $3;
-- Region stats (schema 031): worker-written per-(region,
-- type) book statistics. A completed sweep replaces a region's
-- rows inside one transaction: delete the region, then upsert
-- the fresh measures type by type. The daily table keeps one
-- snapshot row per (region, type, day) for trend work.
-- name: DeleteMarketRegionStatsByRegion :exec
DELETE FROM market_region_stats
WHERE region_id = $1;
-- name: UpsertMarketRegionStat :exec
INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, avg_daily_volume, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (region_id, type_id) DO UPDATE SET
    best_sell    = excluded.best_sell,
    typical_sell = excluded.typical_sell,
    sell_band    = excluded.sell_band,
    best_buy     = excluded.best_buy,
    typical_buy  = excluded.typical_buy,
    buy_band     = excluded.buy_band,
    sell_orders  = excluded.sell_orders,
    buy_orders   = excluded.buy_orders,
    sell_volume  = excluded.sell_volume,
    buy_volume   = excluded.buy_volume,
    avg_daily_volume = excluded.avg_daily_volume,
    updated_at   = excluded.updated_at;
-- name: GetMarketRegionStat :one
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume
FROM market_region_stats
WHERE region_id = $1 AND type_id = $2;
-- name: ListMarketRegionStatsByType :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume
FROM market_region_stats
WHERE type_id = $1
ORDER BY region_id;
-- Fallback stats (schema 054): the Fuzzwork aggregate mirror.
-- One wholesale replacement per orderset inside one
-- transaction (delete, then insert), the guide-prices pattern;
-- readers pick between this table and market_region_stats at
-- read time and never see a half-written snapshot.
-- name: DeleteMarketStatsFallback :exec
DELETE FROM market_stats_fallback;
-- name: InsertMarketStatsFallback :exec
INSERT INTO market_stats_fallback (region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);
-- name: GetMarketStatsFallback :one
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at
FROM market_stats_fallback
WHERE region_id = $1 AND type_id = $2;
-- name: ListMarketStatsFallbackByType :many
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at
FROM market_stats_fallback
WHERE type_id = $1
ORDER BY region_id;
-- name: GetMarketStatsFallbackMeta :one
SELECT id, orderset_id, fetched_at, row_count FROM market_stats_fallback_meta
WHERE id = 1;
-- name: UpsertMarketStatsFallbackMeta :exec
INSERT INTO market_stats_fallback_meta (id, orderset_id, fetched_at, row_count)
VALUES (1, $1, $2, $3)
ON CONFLICT (id) DO UPDATE SET
    orderset_id = excluded.orderset_id,
    fetched_at  = excluded.fetched_at,
    row_count   = excluded.row_count;
-- name: ListMarketRegionStatsByRegion :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume
FROM market_region_stats
WHERE region_id = $1
ORDER BY type_id;
-- name: ListMarketRegionStatsForTypes :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume
FROM market_region_stats
WHERE region_id = $1 AND type_id = ANY($2::bigint[]);
-- name: ListMarketAvgDailyVolumes :many
-- The sold-per-day figure the sweep stores on each region stat:
-- the mean recorded daily volume over the seven calendar days
-- ending on the type's newest recorded day (the newest day and
-- the six before it), which is exactly what historyWindow(rows,
-- 7) averages at render time. One pass over the region's whole
-- history at sweep completion; types with no recorded history
-- have no row here and read as 0.
SELECT h.type_id AS type_id, CAST(AVG(h.volume) AS DOUBLE PRECISION) AS avg_daily_volume
FROM market_history h
WHERE h.region_id = $1
  AND h.date >= (
      SELECT to_char(MAX(m.date)::date - INTERVAL '6 days', 'YYYY-MM-DD')
      FROM market_history m
      WHERE m.region_id = h.region_id AND m.type_id = h.type_id
  )
GROUP BY h.type_id
ORDER BY h.type_id;
-- name: UpsertMarketRegionStatDaily :exec
INSERT INTO market_region_stats_daily (region_id, type_id, day, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (region_id, type_id, day) DO UPDATE SET
    best_sell    = excluded.best_sell,
    typical_sell = excluded.typical_sell,
    sell_band    = excluded.sell_band,
    best_buy     = excluded.best_buy,
    typical_buy  = excluded.typical_buy,
    buy_band     = excluded.buy_band,
    sell_orders  = excluded.sell_orders,
    buy_orders   = excluded.buy_orders,
    sell_volume  = excluded.sell_volume,
    buy_volume   = excluded.buy_volume;
-- name: ListMarketRegionStatsDaily :many
SELECT region_id, type_id, day, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume
FROM market_region_stats_daily
WHERE region_id = $1 AND type_id = $2
ORDER BY day;

-- ---------------------------------------------------------------------
-- Station stats (schema 032): per-(station, type) book
-- statistics written by the same whole-region sweeps as the
-- region stats above. A completed sweep replaces the region's
-- station rows inside the sweep transaction: delete the region,
-- then upsert the fresh measures station by station. The
-- spread scanner page reads only these rows.
-- ---------------------------------------------------------------------
-- name: DeleteMarketStationStatsByRegion :exec
DELETE FROM market_station_stats
WHERE region_id = $1;
-- name: UpsertMarketStationStat :exec
INSERT INTO market_station_stats (location_id, region_id, type_id, best_sell, best_buy, typical_sell, typical_buy, sell_orders, buy_orders, sell_volume, buy_volume, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (location_id, type_id) DO UPDATE SET
    region_id    = excluded.region_id,
    best_sell    = excluded.best_sell,
    best_buy     = excluded.best_buy,
    typical_sell = excluded.typical_sell,
    typical_buy  = excluded.typical_buy,
    sell_orders  = excluded.sell_orders,
    buy_orders   = excluded.buy_orders,
    sell_volume  = excluded.sell_volume,
    buy_volume   = excluded.buy_volume,
    updated_at   = excluded.updated_at;
-- name: ListMarketStationStatsByRegion :many
SELECT location_id, region_id, type_id, best_sell, best_buy, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, typical_sell, typical_buy
FROM market_station_stats
WHERE region_id = $1
ORDER BY location_id, type_id;
-- name: ListScannerOpportunities :many
-- The spread scanner's opportunities, computed and ranked in SQL:
-- one bounded read of at most scannerRowCap rows instead of
-- pulling every stored station row for the region into Go.
-- The spread is priced typical against typical -- the station's
-- stored median per side (schema 053), the same basis the
-- tradefinder routes on -- never the best extremes, which at
-- station grain can be whatever two orders are alone there.
-- A row also needs a book with some depth on both sides (three
-- or more orders a side) and must pass the tradefinder's
-- long-shot cross-check against the region's own typicals when
-- the region has them: a station typical sell more than ten
-- times the region's, or a typical buy under a tenth of it, is
-- a price nobody pays, not an opportunity. Then the usual
-- spread floor, daily-volume floor, and the tradeable size as
-- the least of what trades, what sellers hold, and what buyers
-- want.
SELECT type_id, location_id, typical_sell, typical_buy, sell_volume, buy_volume, daily_volume
FROM (
    SELECT ss.type_id, ss.location_id, ss.typical_sell, ss.typical_buy,
           ss.sell_volume, ss.buy_volume,
           COALESCE(rs.avg_daily_volume, 0) AS daily_volume,
           (ss.typical_sell - ss.typical_buy) * LEAST(COALESCE(rs.avg_daily_volume, 0), ss.sell_volume::double precision, ss.buy_volume::double precision) AS profit
    FROM market_station_stats ss
    LEFT JOIN market_region_stats rs ON rs.region_id = ss.region_id AND rs.type_id = ss.type_id
    WHERE ss.region_id = $1
      AND ss.typical_buy > 0
      AND ss.typical_sell > ss.typical_buy
      AND ss.sell_orders >= 3
      AND ss.buy_orders >= 3
      AND (ss.typical_sell - ss.typical_buy) / ss.typical_buy * 100 >= sqlc.arg(min_spread)
      AND COALESCE(rs.avg_daily_volume, 0) >= sqlc.arg(min_volume)::bigint
      AND LEAST(COALESCE(rs.avg_daily_volume, 0), ss.sell_volume::double precision, ss.buy_volume::double precision) > 0
      AND (COALESCE(rs.typical_sell, 0) <= 0 OR ss.typical_sell <= rs.typical_sell * 10)
      AND (COALESCE(rs.typical_buy, 0) <= 0 OR ss.typical_buy * 10 >= rs.typical_buy)
) ranked
ORDER BY profit DESC, type_id, location_id
LIMIT sqlc.arg(row_cap)::bigint;
-- name: GetMarketStationStatsStamp :one
-- The freshest station-stat write for a region: the scanner
-- page's "prices as of" stamp without pulling the rows.
SELECT MAX(updated_at) AS stamp, COUNT(*) AS row_count
FROM market_station_stats WHERE region_id = $1;
-- name: GetMarketRegionStatsStamp :one
-- The freshest region-stat write for a region: the tradefinder
-- page's "figures last gathered" stamp without pulling rows.
SELECT MAX(updated_at) AS stamp, COUNT(*) AS row_count
FROM market_region_stats WHERE region_id = $1;
-- name: ListTradefinderRoutes :many
-- The tradefinder's routes, computed and ranked in SQL: one
-- bounded read of at most tradefinderRowCap rows. A route buys
-- from the origin's sell orders (its typical sell price) and lists
-- in the destination at its typical sell price. Fresh figures on
-- both sides (3-day rule), margin above zero and over the margin-%
-- floor, long-shot routes (destination over ten times the origin)
-- only on opt-in, the sold-per-day floor, and the movable size as
-- the lesser of what the destination sells a day and the stock on
-- sale in the origin.
SELECT type_id, origin_typical_sell, dest_typical_sell, dest_daily_volume, origin_sell_volume
FROM (
    SELECT o.type_id,
           o.typical_sell AS origin_typical_sell,
           d.typical_sell AS dest_typical_sell,
           d.avg_daily_volume AS dest_daily_volume,
           o.sell_volume AS origin_sell_volume,
           (d.typical_sell - o.typical_sell) * LEAST(d.avg_daily_volume, o.sell_volume::double precision) AS profit
    FROM market_region_stats o
    JOIN market_region_stats d ON d.region_id = sqlc.arg(dest_region) AND d.type_id = o.type_id
    WHERE o.region_id = sqlc.arg(origin_region)
      AND o.typical_sell > 0 AND d.typical_sell > o.typical_sell
      AND (sqlc.arg(include_lowball)::bigint = 1 OR d.typical_sell <= o.typical_sell * 10)
      AND (d.typical_sell - o.typical_sell) / o.typical_sell * 100 >= sqlc.arg(min_margin)
      AND d.avg_daily_volume >= sqlc.arg(min_volume)::bigint
      AND LEAST(d.avg_daily_volume, o.sell_volume::double precision) > 0
      AND o.updated_at >= sqlc.arg(cutoff) AND d.updated_at >= sqlc.arg(cutoff)
) ranked
ORDER BY profit DESC, type_id
LIMIT sqlc.arg(row_cap)::bigint;
-- name: ListTradefinderPendingRoutes :many
-- Routes that pass every price rule but have no sold-per-day
-- figure at the destination yet (no recorded history there): the
-- tradefinder lists them apart and queues their history, ranked
-- by margin times the origin's stock on sale as a rough potential.
SELECT o.type_id,
       o.typical_sell AS origin_typical_sell,
       d.typical_sell AS dest_typical_sell
FROM market_region_stats o
JOIN market_region_stats d ON d.region_id = sqlc.arg(dest_region) AND d.type_id = o.type_id
WHERE o.region_id = sqlc.arg(origin_region)
  AND o.typical_sell > 0 AND d.typical_sell > o.typical_sell
  AND (sqlc.arg(include_lowball)::bigint = 1 OR d.typical_sell <= o.typical_sell * 10)
  AND (d.typical_sell - o.typical_sell) / o.typical_sell * 100 >= sqlc.arg(min_margin)
  AND d.avg_daily_volume = 0
  AND o.sell_volume > 0
  AND o.updated_at >= sqlc.arg(cutoff) AND d.updated_at >= sqlc.arg(cutoff)
ORDER BY (d.typical_sell - o.typical_sell) * o.sell_volume::double precision DESC, o.type_id
LIMIT sqlc.arg(row_cap)::bigint;
-- name: RefreshMarketRegionStatAvgVolume :exec
-- Recomputes one region stat's sold-per-day figure from its stored
-- history (the same seven-day window ListMarketAvgDailyVolumes uses)
-- right after a history fetch, so the figure doesn't wait for the
-- region's next sweep.
UPDATE market_region_stats SET avg_daily_volume = COALESCE((
    SELECT CAST(AVG(h.volume) AS DOUBLE PRECISION)
    FROM market_history h
    WHERE h.region_id = sqlc.arg(region_id) AND h.type_id = sqlc.arg(type_id)
      AND h.date >= (
          SELECT to_char(MAX(m.date)::date - INTERVAL '6 days', 'YYYY-MM-DD')
          FROM market_history m
          WHERE m.region_id = sqlc.arg(region_id) AND m.type_id = sqlc.arg(type_id)
      )
), 0)
WHERE region_id = sqlc.arg(region_id) AND type_id = sqlc.arg(type_id);
-- name: RefreshMarketRegionStatAvgVolumes :execrows
-- RefreshMarketRegionStatAvgVolume for several types of one region in
-- one statement, touching only rows whose figure changed: the
-- tradefinder's self-heal for pending routes whose history is stored
-- but whose sold-per-day figure has not caught up.
UPDATE market_region_stats s SET avg_daily_volume = v.avg_daily_volume
FROM (
    SELECT h.type_id, CAST(AVG(h.volume) AS DOUBLE PRECISION) AS avg_daily_volume
    FROM market_history h
    JOIN (
        SELECT m.type_id, to_char(MAX(m.date)::date - INTERVAL '6 days', 'YYYY-MM-DD') AS window_start
        FROM market_history m
        WHERE m.region_id = sqlc.arg(region_id) AND m.type_id = ANY(sqlc.arg(type_ids)::bigint[])
        GROUP BY m.type_id
    ) w ON w.type_id = h.type_id
    WHERE h.region_id = sqlc.arg(region_id) AND h.type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND h.date >= w.window_start
    GROUP BY h.type_id
) v
WHERE s.region_id = sqlc.arg(region_id) AND s.type_id = v.type_id AND s.avg_daily_volume <> v.avg_daily_volume;
-- name: ListCheapestSellStations :many
-- Per-type cheapest-sell station rows for a bounded type list:
-- the tradefinder's "cheapest at" hints without pulling the
-- region's whole station grain.
SELECT DISTINCT ON (type_id) type_id, location_id, best_sell
FROM market_station_stats
WHERE region_id = $1 AND type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND best_sell > 0
ORDER BY type_id, best_sell, location_id;
-- name: ListBestBuyStations :many
-- Per-type highest-buy station rows for a bounded type list:
-- the tradefinder's "best buyer at" hints without pulling the
-- region's whole station grain.
SELECT DISTINCT ON (type_id) type_id, location_id, best_buy
FROM market_station_stats
WHERE region_id = $1 AND type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND best_buy > 0
ORDER BY type_id, best_buy DESC, location_id;

-- ---------------------------------------------------------------------
-- Order lifecycle (schema 033): append-only per-order history
-- distilled from order snapshots. The worker upserts open orders,
-- closes rows whose order has left the snapshot, and prunes old
-- closed rows; the Orders page reads only these rows.
-- ---------------------------------------------------------------------
-- name: GetOrderLifecycle :one
SELECT *
FROM order_lifecycle
WHERE character_id = $1 AND order_id = $2;
-- name: UpsertOrderLifecycle :exec
INSERT INTO order_lifecycle (character_id, order_id, type_id, location_id, region_id, is_buy_order, listed_price, volume_total, volume_remain_last, first_seen_at, last_seen_at, closed_at, close_kind, outbid_events, beaten_now)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULL, '', 0, 0)
ON CONFLICT (character_id, order_id) DO UPDATE SET
    type_id            = excluded.type_id,
    location_id        = excluded.location_id,
    region_id          = excluded.region_id,
    is_buy_order       = excluded.is_buy_order,
    listed_price       = excluded.listed_price,
    volume_total       = excluded.volume_total,
    volume_remain_last = excluded.volume_remain_last,
    last_seen_at       = excluded.last_seen_at;
-- name: UpdateOrderLifecycleBeaten :exec
UPDATE order_lifecycle
SET beaten_now = $1, outbid_events = $2
WHERE character_id = $3 AND order_id = $4 AND closed_at IS NULL;
-- name: CloseOrderLifecycle :exec
UPDATE order_lifecycle
SET closed_at = sqlc.arg(closed_at)::timestamptz, close_kind = sqlc.arg(close_kind), beaten_now = 0
WHERE character_id = sqlc.arg(character_id) AND order_id = sqlc.arg(order_id) AND closed_at IS NULL;
-- name: ListOrderLifecycleByCharacter :many
SELECT *
FROM order_lifecycle
WHERE character_id = $1
ORDER BY first_seen_at DESC, order_id DESC;
-- name: ListOpenOrderLifecycleByCharacter :many
SELECT *
FROM order_lifecycle
WHERE character_id = $1 AND closed_at IS NULL
ORDER BY order_id;
-- name: ListOrderLifecycleByUser :many
SELECT ol.*
FROM order_lifecycle ol
JOIN characters c ON c.character_id = ol.character_id
WHERE c.user_id = $1
ORDER BY ol.closed_at DESC NULLS LAST, ol.first_seen_at DESC, ol.order_id DESC;
-- name: ListClosedOrderLifecycleByCharacter :many
SELECT *
FROM order_lifecycle
WHERE character_id = $1 AND closed_at IS NOT NULL
ORDER BY closed_at DESC, order_id DESC
LIMIT sqlc.arg(row_limit)::bigint;
-- name: PruneOldOrderLifecycle :exec
DELETE FROM order_lifecycle
WHERE id IN (
    SELECT ol.id FROM order_lifecycle AS ol
    WHERE ol.closed_at < sqlc.arg(closed_before)::timestamptz
    ORDER BY ol.closed_at
    LIMIT sqlc.arg(row_limit)::bigint
);

-- ---------------------------------------------------------------------
-- Sweep staging (schema 034): disk-staged whole-region
-- sweeps. A market_sweep_state row means a sweep is in progress
-- for that region and next_page is its resume cursor; each
-- fetched page's orders land in market_sweep_orders in the same
-- transaction that advances the cursor. At completion the
-- staged book is distilled into the region/station stats above
-- and the staging rows are deleted in that same transaction.
-- ---------------------------------------------------------------------
-- name: GetMarketSweepState :one
SELECT region_id, next_page, pages_total, started_at, updated_at
FROM market_sweep_state
WHERE region_id = $1;
-- name: InsertMarketSweepState :exec
INSERT INTO market_sweep_state (region_id, next_page, pages_total, started_at, updated_at)
VALUES ($1, 1, 0, $2, $3);
-- name: UpdateMarketSweepState :exec
UPDATE market_sweep_state
SET next_page = $1, pages_total = $2, updated_at = $3
WHERE region_id = $4;
-- name: DeleteMarketSweepState :exec
DELETE FROM market_sweep_state
WHERE region_id = $1;
-- name: InsertMarketSweepOrder :exec
INSERT INTO market_sweep_orders (region_id, type_id, is_buy_order, price, volume_remain, location_id, system_id)
VALUES ($1, $2, $3, $4, $5, $6, $7);
-- name: DeleteMarketSweepOrdersByRegion :exec
DELETE FROM market_sweep_orders
WHERE region_id = $1;
-- name: ListMarketSweepTypeIDs :many
SELECT DISTINCT type_id
FROM market_sweep_orders
WHERE region_id = $1
ORDER BY type_id;
-- name: ListMarketSweepTypeOrders :many
SELECT is_buy_order, price, volume_remain, location_id, system_id
FROM market_sweep_orders
WHERE region_id = $1 AND type_id = $2
ORDER BY is_buy_order, price;

-- ---------------------------------------------------------------------
-- Stored book (schema 050): the top orders per side a completed
-- sweep keeps, so the Market page can show an order book to
-- visitors who never fetch (anonymous readers among them). The
-- sweep completion transaction replaces a region's rows whole,
-- exactly like the region stats distilled beside them; figures
-- over the whole book (best, typical, depth) stay on
-- market_region_stats, these rows are only the displayed top.
-- ---------------------------------------------------------------------
-- name: DeleteMarketBookOrdersByRegion :exec
DELETE FROM market_book_orders
WHERE region_id = $1;
-- name: InsertMarketBookOrder :exec
INSERT INTO market_book_orders (region_id, type_id, is_buy_order, rank, price, volume_remain, location_id, system_id, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
-- name: ListMarketBookOrders :many
SELECT region_id, type_id, is_buy_order, rank, price, volume_remain, location_id, system_id, updated_at
FROM market_book_orders
WHERE region_id = $1 AND type_id = $2
ORDER BY is_buy_order, rank;

-- ---------------------------------------------------------------------
-- Site settings (schema 050): live operator switches, one row
-- per key, flipped from the Admin page without a restart. An
-- environment variable may carry the boot default; the stored
-- row wins once it exists.
-- ---------------------------------------------------------------------
-- name: GetSiteSetting :one
SELECT key, value, updated_at
FROM site_settings
WHERE key = $1;
-- name: UpsertSiteSetting :exec
INSERT INTO site_settings (key, value, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (key) DO UPDATE SET
    value      = excluded.value,
    updated_at = excluded.updated_at;
-- name: ListMarketSweepStationAggregates :many
SELECT location_id, type_id, is_buy_order,
       CAST(MIN(price) AS DOUBLE PRECISION) AS min_price, CAST(MAX(price) AS DOUBLE PRECISION) AS max_price,
       COUNT(*) AS order_count, CAST(SUM(volume_remain) AS BIGINT) AS total_volume
FROM market_sweep_orders
WHERE region_id = $1 AND location_id > 0
GROUP BY location_id, type_id, is_buy_order
ORDER BY location_id, type_id, is_buy_order;

-- ---------------------------------------------------------------------
-- Station leaderboard (schema_pg 002): per-station open-order
-- counts and open ISK value per side, distilled by the same
-- whole-region sweeps as the stats above. A completed sweep
-- replaces the region's leaderboard rows inside the sweep
-- transaction: delete the region, then upsert the fresh measures
-- station by station. The aggregates read the staged book like
-- the station stats do, but grouped only by place: the open
-- value needs price times remaining volume per order, which the
-- type-grain aggregates cannot reconstruct. The leaderboard page
-- reads only these rows. A region argument of 0 means every
-- region.
-- ---------------------------------------------------------------------
-- name: ListMarketSweepStationLeaderboardAggregates :many
SELECT location_id, is_buy_order,
       COUNT(*) AS order_count, CAST(SUM(price * volume_remain) AS DOUBLE PRECISION) AS open_value
FROM market_sweep_orders
WHERE region_id = $1 AND location_id > 0
GROUP BY location_id, is_buy_order
ORDER BY location_id, is_buy_order;
-- name: DeleteMarketStationLeaderboardByRegion :exec
DELETE FROM market_station_leaderboard
WHERE region_id = $1;
-- name: UpsertMarketStationLeaderboard :exec
INSERT INTO market_station_leaderboard (region_id, location_id, sell_orders, buy_orders, sell_value, buy_value, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (region_id, location_id) DO UPDATE SET
    sell_orders = excluded.sell_orders,
    buy_orders  = excluded.buy_orders,
    sell_value  = excluded.sell_value,
    buy_value   = excluded.buy_value,
    updated_at  = excluded.updated_at;
-- name: ListMarketStationLeaderboard :many
SELECT region_id, location_id, sell_orders, buy_orders, sell_value, buy_value, updated_at
FROM market_station_leaderboard
WHERE ($1::bigint = 0 OR region_id = $1::bigint)
ORDER BY region_id, location_id;
-- name: GetMarketStationLeaderboardStamp :one
SELECT MAX(updated_at) AS stamp
FROM market_station_leaderboard
WHERE ($1::bigint = 0 OR region_id = $1::bigint);

-- Per-type market price TTL cache. The worker refreshes
-- only rows older than the TTL; pages read cache-only.
-- name: UpsertMarketTypePrice :exec
INSERT INTO market_type_prices (region_id, type_id, buy_price, sell_price, buy_volume, sell_volume, fetched_at)
VALUES ($1, $2, $3, $4, $5, $6, now())
ON CONFLICT (region_id, type_id) DO UPDATE SET
    buy_price   = excluded.buy_price,
    sell_price  = excluded.sell_price,
    buy_volume  = excluded.buy_volume,
    sell_volume = excluded.sell_volume,
    fetched_at  = excluded.fetched_at;
-- name: GetMarketTypePrice :one
SELECT region_id, type_id, buy_price, sell_price, buy_volume, sell_volume, fetched_at
FROM market_type_prices
WHERE region_id = $1 AND type_id = $2;
-- name: ListStaleMarketTypePrices :many
SELECT region_id, type_id
FROM market_type_prices
WHERE fetched_at < now() - ($1::int * INTERVAL '1 second')
ORDER BY fetched_at ASC
LIMIT $2;
-- name: ListMarketTypePricesForTypes :many
SELECT region_id, type_id, buy_price, sell_price, buy_volume, sell_volume, fetched_at
FROM market_type_prices
WHERE region_id = $1 AND type_id = ANY($2::bigint[]);

-- Industry cost indices per (system, activity).
-- name: UpsertIndustryCostIndex :exec
INSERT INTO industry_cost_indices (solar_system_id, activity, cost_index, fetched_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (solar_system_id, activity) DO UPDATE SET
    cost_index = excluded.cost_index,
    fetched_at = excluded.fetched_at;
-- name: ListIndustryCostIndices :many
SELECT solar_system_id, activity, cost_index, fetched_at
FROM industry_cost_indices
ORDER BY activity, cost_index ASC;
-- name: GetIndustryCostIndex :one
SELECT solar_system_id, activity, cost_index, fetched_at
FROM industry_cost_indices
WHERE solar_system_id = $1 AND activity = $2;
-- name: GetIndustryCostIndicesStamp :one
-- The freshest cost-index write: the refresh gate (an hour's
-- cadence when ESI's Expires is missing) without pulling rows.
SELECT MAX(fetched_at) AS stamp, COUNT(*) AS row_count
FROM industry_cost_indices;

-- Restock planner targets.
-- name: UpsertRestockTarget :exec
INSERT INTO restock_targets (user_id, type_id, target_qty, min_margin_pct, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, type_id) DO UPDATE SET
    target_qty     = excluded.target_qty,
    min_margin_pct = excluded.min_margin_pct,
    updated_at     = excluded.updated_at;
-- name: ListRestockTargets :many
SELECT user_id, type_id, target_qty, min_margin_pct, updated_at
FROM restock_targets
WHERE user_id = $1
ORDER BY type_id;
-- name: DeleteRestockTarget :exec
DELETE FROM restock_targets
WHERE user_id = $1 AND type_id = $2;
