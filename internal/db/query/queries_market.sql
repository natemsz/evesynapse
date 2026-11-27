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
-- name: UpsertMarketHistoryDays :exec
-- One pair's whole history payload in one statement: ESI answers
-- about a year of days per pair, and a statement per day was most
-- of the market pass's time. The days must be distinct.
INSERT INTO market_history (region_id, type_id, date, average, highest, lowest, volume, order_count)
SELECT sqlc.arg(region_id)::bigint, sqlc.arg(type_id)::bigint,
    unnest(sqlc.arg(dates)::text[]), unnest(sqlc.arg(averages)::float8[]),
    unnest(sqlc.arg(highests)::float8[]), unnest(sqlc.arg(lowests)::float8[]),
    unnest(sqlc.arg(volumes)::bigint[]), unnest(sqlc.arg(order_counts)::bigint[])
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
-- name: ListHistoryFetchableTypeIDs :many
-- The types ESI market history can know: every published SDE
-- type, plus every type the region sweeps have stored stats
-- for (a type ESI served orders for has history to ask for,
-- published or not). A candidate outside this set can only
-- be answered 404.
SELECT type_id FROM sde_types WHERE published = 1
UNION
SELECT type_id FROM market_region_stats;
-- name: GetMarketFetchState :one
SELECT kind, state, detail, attempted_at
FROM market_fetch_state
WHERE kind = $1;
-- name: ListMarketFetchStatesByKinds :many
-- The fetch states of many kinds in one read, for a pass that
-- gates thousands of candidates (a kind with no row is absent).
SELECT kind, state, detail, attempted_at
FROM market_fetch_state
WHERE kind = ANY(sqlc.arg(kinds)::text[]);
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

-- name: ListStructureProbes :many
-- Characters already refused for one structure within the window.
SELECT character_id
FROM structure_probes
WHERE structure_id = sqlc.arg(structure_id)::bigint
  AND asked_at >= sqlc.arg(since)::timestamptz;

-- name: RecordStructureProbe :exec
INSERT INTO structure_probes (structure_id, character_id, asked_at)
VALUES (sqlc.arg(structure_id)::bigint, sqlc.arg(character_id)::bigint, sqlc.arg(asked_at)::timestamptz)
ON CONFLICT (structure_id, character_id) DO UPDATE SET asked_at = EXCLUDED.asked_at;

-- name: ClearStructureProbes :exec
DELETE FROM structure_probes WHERE structure_id = sqlc.arg(structure_id)::bigint;

-- name: PruneStructureProbes :exec
DELETE FROM structure_probes WHERE asked_at < sqlc.arg(before)::timestamptz;

-- name: ListResolvedStructureNames :many
SELECT structure_id, name
FROM structure_names
WHERE state = 'resolved' AND name != ''
ORDER BY structure_id;

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
-- unresolved planet id, resolved in the background from the static
-- data (sde_celestials). 'resolved' rows
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

-- Settled records for a set of pilots at once: a roster's
-- dossiers, read in one bounded pass.
-- name: ListPilotRecordsByIDs :many
SELECT character_id, payload, state, fetched_at
FROM pilot_records
WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])
  AND state = 'ready'
ORDER BY character_id
LIMIT sqlc.arg(max_rows)::bigint;
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
INSERT INTO market_region_stats (region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, avg_daily_volume, updated_at, swept_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
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
    updated_at   = excluded.updated_at,
    swept_at     = excluded.swept_at;
-- name: TouchMarketRegionStatsSweptAt :execrows
-- Stamps the last-verified time (schema 184) on the rows a
-- completed sweep fetched, changed or not: updated_at keeps
-- its last-changed meaning, swept_at is what the freshness
-- checks read.
UPDATE market_region_stats SET swept_at = sqlc.arg(swept_at)
WHERE region_id = sqlc.arg(region_id) AND type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- name: GetMarketRegionStat :one
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev,
       sell_chg_1d, sell_chg_7d, sell_chg_30d, buy_chg_1d, buy_chg_7d, buy_chg_30d,
       volatility_30d, history_days, avg_daily_volume_30d,
       sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d, swept_at,
       sell_avg_7d, sell_median_7d, sell_median_30d, mover_chg_7v30, mover_hold_days,
       isk_traded_7d, avg_orders_7d, avg_vol_7d, avg_vol_30d,
       mover_score, isk_move_7d, daily_spread_profit, spread_pct, spread_widened, confirmed_hubs
FROM market_region_stats
WHERE region_id = $1 AND type_id = $2;
-- name: ListMarketRegionStatsByType :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev,
       sell_chg_1d, sell_chg_7d, sell_chg_30d, buy_chg_1d, buy_chg_7d, buy_chg_30d,
       volatility_30d, history_days, avg_daily_volume_30d,
       sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d, swept_at,
       sell_avg_7d, sell_median_7d, sell_median_30d, mover_chg_7v30, mover_hold_days,
       isk_traded_7d, avg_orders_7d, avg_vol_7d, avg_vol_30d,
       mover_score, isk_move_7d, daily_spread_profit, spread_pct, spread_widened, confirmed_hubs
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
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at,
       wavg_sell, min_sell, max_sell, stddev_sell, wavg_buy, min_buy, max_buy, stddev_buy
FROM market_stats_fallback
WHERE region_id = $1 AND type_id = $2;
-- name: ListMarketStatsFallbackByType :many
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at,
       wavg_sell, min_sell, max_sell, stddev_sell, wavg_buy, min_buy, max_buy, stddev_buy
FROM market_stats_fallback
WHERE type_id = $1
ORDER BY region_id;
-- name: ListMarketStatsFallbackForTypes :many
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at,
       wavg_sell, min_sell, max_sell, stddev_sell, wavg_buy, min_buy, max_buy, stddev_buy
FROM market_stats_fallback
WHERE region_id = $1 AND type_id = ANY($2::bigint[])
ORDER BY type_id;

-- name: ListStatPackFallbackStats :many
SELECT region_id, type_id, median_sell, fivepct_sell, median_buy, fivepct_buy, sell_orders, buy_orders, sell_volume, buy_volume, source, orderset_id, source_asof, fetched_at,
       wavg_sell, min_sell, max_sell, stddev_sell, wavg_buy, min_buy, max_buy, stddev_buy
FROM market_stats_fallback;
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
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev,
       sell_chg_1d, sell_chg_7d, sell_chg_30d, buy_chg_1d, buy_chg_7d, buy_chg_30d,
       volatility_30d, history_days, avg_daily_volume_30d,
       sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d, swept_at,
       sell_avg_7d, sell_median_7d, sell_median_30d, mover_chg_7v30, mover_hold_days,
       isk_traded_7d, avg_orders_7d, avg_vol_7d, avg_vol_30d,
       mover_score, isk_move_7d, daily_spread_profit, spread_pct, spread_widened, confirmed_hubs
FROM market_region_stats
WHERE region_id = $1
ORDER BY type_id;
-- name: ListMarketRegionStatsForTypes :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev,
       sell_chg_1d, sell_chg_7d, sell_chg_30d, buy_chg_1d, buy_chg_7d, buy_chg_30d,
       volatility_30d, history_days, avg_daily_volume_30d,
       sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d, swept_at,
       sell_avg_7d, sell_median_7d, sell_median_30d, mover_chg_7v30, mover_hold_days,
       isk_traded_7d, avg_orders_7d, avg_vol_7d, avg_vol_30d,
       mover_score, isk_move_7d, daily_spread_profit, spread_pct, spread_widened, confirmed_hubs
FROM market_region_stats
WHERE region_id = $1 AND type_id = ANY($2::bigint[]);
-- All swept region stats for the in-memory stat pack snapshot
-- (market_stat_pack.go): loaded once at startup and kept resident.
-- name: ListStatPackRegionStats :many
SELECT region_id, type_id, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, avg_daily_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev,
       sell_chg_1d, sell_chg_7d, sell_chg_30d, buy_chg_1d, buy_chg_7d, buy_chg_30d,
       volatility_30d, history_days, avg_daily_volume_30d,
       sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d, swept_at,
       sell_avg_7d, sell_median_7d, sell_median_30d, mover_chg_7v30, mover_hold_days,
       isk_traded_7d, avg_orders_7d, avg_vol_7d, avg_vol_30d,
       mover_score, isk_move_7d, daily_spread_profit, spread_pct, spread_widened, confirmed_hubs
FROM market_region_stats;
-- All station stats for the in-memory stat pack snapshot.
-- name: ListStatPackStationStats :many
SELECT location_id, region_id, type_id, best_sell, best_buy, sell_orders, buy_orders, sell_volume, buy_volume, updated_at, typical_sell, typical_buy
FROM market_station_stats;
-- All recorded best beats for the in-memory stat pack snapshot.
-- name: ListStatPackBestBeats :many
SELECT location_id, region_id, type_id, is_buy_order, at
FROM market_best_beats;
-- name: ListMarketRegionAvgVolumes :many
-- The sold-per-day figure each of a region's stats rows carries:
-- kept current at history ingest (RefreshMarketRegionStatAvgVolume),
-- so the sweep reads it instead of recomputing it from the whole
-- history table.
SELECT type_id, avg_daily_volume FROM market_region_stats WHERE region_id = $1;
-- name: ListMarketAvgDailyVolumesForTypes :many
-- The sold-per-day figure for types a region's book has just gained:
-- the mean recorded daily volume over the seven calendar days
-- ending on the type's newest recorded day, read by key per type.
-- Types with no recorded history have no row and read as 0.
SELECT h.type_id AS type_id, CAST(AVG(h.volume) AS DOUBLE PRECISION) AS avg_daily_volume
FROM market_history h
JOIN (
    SELECT m.type_id, to_char(MAX(m.date)::date - INTERVAL '6 days', 'YYYY-MM-DD') AS window_start
    FROM market_history m
    WHERE m.region_id = sqlc.arg(region_id) AND m.type_id = ANY(sqlc.arg(type_ids)::bigint[])
    GROUP BY m.type_id
) w ON w.type_id = h.type_id
WHERE h.region_id = sqlc.arg(region_id) AND h.type_id = ANY(sqlc.arg(type_ids)::bigint[])
  AND h.date >= w.window_start
GROUP BY h.type_id;
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
SELECT region_id, type_id, day, best_sell, typical_sell, sell_band, best_buy, typical_buy, buy_band, sell_orders, buy_orders, sell_volume, buy_volume,
       sell_avg, sell_max, sell_p5, sell_stddev, buy_avg, buy_min, buy_p5, buy_stddev
FROM market_region_stats_daily
WHERE region_id = $1 AND type_id = $2
ORDER BY day;
-- name: RefreshMarketStatPackSparks :execrows
-- The stat pack's 30-day price line (schema 121), derived for one
-- region inside the sweep's transaction right after the trend
-- figures: the typical sell of the daily rows from the last 30
-- days as polyline points in the 100 by 28 sparkline box (2 units
-- of padding, flat lines at mid height), with their low, high and
-- mean. A type with under two such days gets an empty line.
-- type_ids narrows it to the items a sweep wrote; empty means all.
UPDATE market_region_stats s SET
    sell_spark_30d = COALESCE(t.pts, ''),
    sell_low_30d   = COALESCE(t.lo, 0),
    sell_high_30d  = COALESCE(t.hi, 0),
    sell_mean_30d  = COALESCE(t.mean, 0)
FROM (
    SELECT r.type_id, p.pts, p.lo, p.hi, p.mean
    FROM market_region_stats r
    LEFT JOIN (
        SELECT x.type_id,
               MIN(x.lo) AS lo, MIN(x.hi) AS hi, AVG(x.v) AS mean,
               string_agg(
                   to_char(2 + x.rn * 96.0 / (x.n - 1), 'FM9990.0') || ',' ||
                   to_char(CASE WHEN x.hi > x.lo THEN 26 - (x.v - x.lo) / (x.hi - x.lo) * 24 ELSE 14 END, 'FM9990.0'),
                   ' ' ORDER BY x.rn) AS pts
        FROM (
            SELECT dd.type_id, dd.typical_sell AS v,
                   ROW_NUMBER() OVER w - 1 AS rn,
                   COUNT(*) OVER (PARTITION BY dd.type_id) AS n,
                   MIN(dd.typical_sell) OVER (PARTITION BY dd.type_id) AS lo,
                   MAX(dd.typical_sell) OVER (PARTITION BY dd.type_id) AS hi
            FROM market_region_stats_daily dd
            WHERE dd.region_id = sqlc.arg(region_id)
              AND dd.day > to_char(sqlc.arg(today)::date - 30, 'YYYY-MM-DD')
              AND dd.typical_sell > 0
              AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR dd.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
            WINDOW w AS (PARTITION BY dd.type_id ORDER BY dd.day)
        ) x
        WHERE x.n >= 2
        GROUP BY x.type_id
    ) p ON p.type_id = r.type_id
    WHERE r.region_id = sqlc.arg(region_id)
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR r.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
) t
WHERE s.region_id = sqlc.arg(region_id) AND s.type_id = t.type_id;

-- name: ListMarketStatSparks :many
-- The stored 30-day price lines for the types a page lists, in one
-- region: one bounded read per region for a whole table's hover
-- cards.
SELECT type_id, sell_spark_30d, sell_low_30d, sell_high_30d, sell_mean_30d
FROM market_region_stats
WHERE region_id = sqlc.arg(region_id) AND type_id = ANY(sqlc.arg(type_ids)::bigint[])
  AND sell_spark_30d <> '';

-- name: RefreshMarketStatPackTrends :execrows
-- The stat pack's trend and mover figures (schemas 101, 188, 189),
-- derived for one region inside the sweep's own transaction, right
-- after today's daily rows land.
--
--   * 1d and 30d changes compare live typical prices against the
--     newest daily row inside their comparison window (1-3d, 30-35d).
--   * 7d change compares the rolling 7-day average typical price
--     ([today-6..today]) against the prior 7-day rolling average
--     ([today-13..today-7]), avoiding single-day snapshot noise.
--   * sell_median_7d, sell_median_30d, mover_chg_7v30 and
--     mover_hold_days read actual traded daily averages from
--     market_history over the trailing 7 and 30 days.
--   * isk_traded_7d, avg_orders_7d, avg_vol_7d and avg_vol_30d read
--     calendar-day averages from regional market_history (actual
--     recorded volume * average price and order_count).
--   * mover_score, isk_move_7d, daily_spread_profit, spread_pct and
--     spread_widened are stored so Trends and Market Movers never
--     compute scores at page load.
--
-- type_ids narrows it to the items a sweep wrote; empty means all.
WITH cur7 AS (
    SELECT dd.type_id,
           AVG(dd.typical_sell) FILTER (WHERE dd.typical_sell > 0)::double precision AS sell_avg_7d,
           AVG(dd.typical_buy) FILTER (WHERE dd.typical_buy > 0)::double precision AS buy_avg_7d
    FROM market_region_stats_daily dd
    WHERE dd.region_id = sqlc.arg(region_id)
      AND dd.day >= to_char(sqlc.arg(today)::date - 6, 'YYYY-MM-DD')
      AND dd.day <= to_char(sqlc.arg(today)::date, 'YYYY-MM-DD')
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR dd.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY dd.type_id
),
prev7 AS (
    SELECT dd.type_id,
           AVG(dd.typical_sell) FILTER (WHERE dd.typical_sell > 0)::double precision AS sell_avg_prev7,
           AVG(dd.typical_buy) FILTER (WHERE dd.typical_buy > 0)::double precision AS buy_avg_prev7
    FROM market_region_stats_daily dd
    WHERE dd.region_id = sqlc.arg(region_id)
      AND dd.day >= to_char(sqlc.arg(today)::date - 13, 'YYYY-MM-DD')
      AND dd.day <= to_char(sqlc.arg(today)::date - 7, 'YYYY-MM-DD')
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR dd.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY dd.type_id
),
spread30 AS (
    SELECT dd.type_id,
           PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY (dd.typical_sell - dd.typical_buy) / dd.typical_buy * 100.0)
               FILTER (WHERE dd.typical_buy > 0 AND dd.typical_sell > dd.typical_buy)::double precision AS spread_median_30d
    FROM market_region_stats_daily dd
    WHERE dd.region_id = sqlc.arg(region_id)
      AND dd.day >= to_char(sqlc.arg(today)::date - 29, 'YYYY-MM-DD')
      AND dd.day <= to_char(sqlc.arg(today)::date, 'YYYY-MM-DD')
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR dd.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY dd.type_id
),
hist_ref AS (
    SELECT mh.type_id,
           CASE WHEN MAX(mh.date)::date >= sqlc.arg(today)::date - 2
                 AND MAX(mh.date)::date <= sqlc.arg(today)::date
                THEN MAX(mh.date)::date
                ELSE sqlc.arg(today)::date - 1
           END AS ref_date,
           MAX(mh.date)::date AS max_date
    FROM market_history mh
    WHERE mh.region_id = sqlc.arg(region_id)
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR mh.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY mh.type_id
),
hist AS (
    SELECT mh.type_id,
           COALESCE(AVG(mh.volume) FILTER (WHERE mh.date >= to_char(hr.max_date - 29, 'YYYY-MM-DD')), 0)::double precision AS vol30_legacy,
           COALESCE(SUM(mh.volume::double precision * mh.average)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS isk_traded_7d,
           COALESCE(SUM(mh.order_count::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_orders_7d,
           COALESCE(SUM(mh.volume::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_vol_7d,
           COALESCE(SUM(mh.volume::double precision)
               FILTER (WHERE mh.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 30.0 AS avg_vol_30d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY mh.average)
               FILTER (WHERE mh.average > 0 AND mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_7d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY mh.average)
               FILTER (WHERE mh.average > 0 AND mh.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_30d
    FROM market_history mh
    JOIN hist_ref hr ON hr.type_id = mh.type_id
    WHERE mh.region_id = sqlc.arg(region_id)
      AND mh.date >= to_char(LEAST(hr.ref_date - 29, hr.max_date - 29), 'YYYY-MM-DD')
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR mh.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY mh.type_id
),
hold AS (
    SELECT mh.type_id,
           COUNT(*) FILTER (WHERE
               (h.sell_median_7d > h.sell_median_30d
                AND mh.average >= h.sell_median_30d + 0.5 * (h.sell_median_7d - h.sell_median_30d))
            OR (h.sell_median_7d < h.sell_median_30d
                AND mh.average <= h.sell_median_30d + 0.5 * (h.sell_median_7d - h.sell_median_30d))
           )::bigint AS hold_days
    FROM market_history mh
    JOIN hist_ref hr ON hr.type_id = mh.type_id
    JOIN hist h ON h.type_id = mh.type_id
    WHERE mh.region_id = sqlc.arg(region_id)
      AND mh.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD')
      AND mh.date <= to_char(hr.ref_date, 'YYYY-MM-DD')
      AND mh.average > 0
      AND h.sell_median_7d > 0
      AND h.sell_median_30d > 0
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR mh.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY mh.type_id
)
UPDATE market_region_stats s SET
    sell_chg_1d          = CASE WHEN s.typical_sell > 0 AND t.s1 > 0 THEN (s.typical_sell - t.s1) / t.s1 * 100.0 END,
    sell_chg_7d          = CASE WHEN t.sell_avg_7d > 0 AND t.sell_avg_prev7 > 0 THEN (t.sell_avg_7d - t.sell_avg_prev7) / t.sell_avg_prev7 * 100.0 END,
    sell_chg_30d         = CASE WHEN s.typical_sell > 0 AND t.s30 > 0 THEN (s.typical_sell - t.s30) / t.s30 * 100.0 END,
    buy_chg_1d           = CASE WHEN s.typical_buy > 0 AND t.b1 > 0 THEN (s.typical_buy - t.b1) / t.b1 * 100.0 END,
    buy_chg_7d           = CASE WHEN t.buy_avg_7d > 0 AND t.buy_avg_prev7 > 0 THEN (t.buy_avg_7d - t.buy_avg_prev7) / t.buy_avg_prev7 * 100.0 END,
    buy_chg_30d          = CASE WHEN s.typical_buy > 0 AND t.b30 > 0 THEN (s.typical_buy - t.b30) / t.b30 * 100.0 END,
    volatility_30d       = t.vol,
    history_days         = t.days,
    avg_daily_volume_30d = t.vol30_legacy,
    sell_avg_7d          = COALESCE(t.sell_avg_7d, 0),
    sell_median_7d       = COALESCE(t.sell_median_7d, 0),
    sell_median_30d      = COALESCE(t.sell_median_30d, 0),
    mover_chg_7v30       = t.mover_chg_7v30,
    mover_hold_days      = t.hold_days,
    isk_traded_7d        = t.isk_traded_7d,
    avg_orders_7d        = t.avg_orders_7d,
    avg_vol_7d           = t.avg_vol_7d,
    avg_vol_30d          = t.avg_vol_30d,
    mover_score          = CASE WHEN t.mover_chg_7v30 IS NOT NULL THEN ABS(t.mover_chg_7v30) * LN(1.0 + GREATEST(t.isk_traded_7d, 0)) END,
    isk_move_7d          = CASE WHEN COALESCE(t.sell_median_7d, 0) > 0 AND COALESCE(t.sell_median_30d, 0) > 0
                                THEN ABS(t.sell_median_7d - t.sell_median_30d) * GREATEST(t.avg_vol_7d, 0)
                                ELSE 0 END,
    daily_spread_profit  = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                                THEN (s.typical_sell - s.typical_buy) * LEAST(GREATEST(t.avg_vol_7d, 0), s.sell_volume::double precision, s.buy_volume::double precision)
                                ELSE 0 END,
    spread_pct           = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                                THEN (s.typical_sell - s.typical_buy) / s.typical_buy * 100.0 END,
    spread_widened       = (s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                            AND ((s.typical_sell - s.typical_buy) / s.typical_buy * 100.0) >= 10.0
                            AND COALESCE(t.base_spread, 0) > 0
                            AND ((s.typical_sell - s.typical_buy) / s.typical_buy * 100.0) >= 2.0 * t.base_spread)
FROM (
    SELECT r.type_id,
           d1.typical_sell AS s1, d1.typical_buy AS b1,
           d30.typical_sell AS s30, d30.typical_buy AS b30,
           c7.sell_avg_7d, c7.buy_avg_7d,
           p7.sell_avg_prev7, p7.buy_avg_prev7,
           COALESCE(h.sell_median_7d, 0)::double precision AS sell_median_7d,
           COALESCE(h.sell_median_30d, 0)::double precision AS sell_median_30d,
           CASE WHEN COALESCE(h.sell_median_7d, 0) > 0 AND COALESCE(h.sell_median_30d, 0) > 0
                THEN (h.sell_median_7d - h.sell_median_30d) / h.sell_median_30d * 100.0
           END AS mover_chg_7v30,
           COALESCE(hd.hold_days, 0)::bigint AS hold_days,
           COALESCE(sp30.spread_median_30d,
                    CASE WHEN d1.typical_buy > 0 AND d1.typical_sell > d1.typical_buy
                         THEN (d1.typical_sell - d1.typical_buy) / d1.typical_buy * 100.0 END) AS base_spread,
           v.vol, COALESCE(v.days, 0) AS days,
           COALESCE(h.vol30_legacy, 0)::double precision AS vol30_legacy,
           COALESCE(h.isk_traded_7d, 0)::double precision AS isk_traded_7d,
           COALESCE(h.avg_orders_7d, 0)::double precision AS avg_orders_7d,
           COALESCE(h.avg_vol_7d, 0)::double precision AS avg_vol_7d,
           COALESCE(h.avg_vol_30d, 0)::double precision AS avg_vol_30d
    FROM market_region_stats r
    LEFT JOIN cur7 c7 ON c7.type_id = r.type_id
    LEFT JOIN prev7 p7 ON p7.type_id = r.type_id
    LEFT JOIN spread30 sp30 ON sp30.type_id = r.type_id
    LEFT JOIN hold hd ON hd.type_id = r.type_id
    LEFT JOIN LATERAL (
        SELECT dd.typical_sell, dd.typical_buy FROM market_region_stats_daily dd
        WHERE dd.region_id = r.region_id AND dd.type_id = r.type_id
          AND dd.day <= to_char(sqlc.arg(today)::date - 1, 'YYYY-MM-DD')
          AND dd.day >= to_char(sqlc.arg(today)::date - 3, 'YYYY-MM-DD')
        ORDER BY dd.day DESC LIMIT 1
    ) d1 ON true
    LEFT JOIN LATERAL (
        SELECT dd.typical_sell, dd.typical_buy FROM market_region_stats_daily dd
        WHERE dd.region_id = r.region_id AND dd.type_id = r.type_id
          AND dd.day <= to_char(sqlc.arg(today)::date - 30, 'YYYY-MM-DD')
          AND dd.day >= to_char(sqlc.arg(today)::date - 35, 'YYYY-MM-DD')
        ORDER BY dd.day DESC LIMIT 1
    ) d30 ON true
    LEFT JOIN (
        SELECT x.type_id,
               CASE WHEN COUNT(x.ret) >= 7 THEN STDDEV_SAMP(x.ret) * 100 END AS vol,
               COUNT(*) AS days
        FROM (
            SELECT dd.type_id,
                   LN(dd.typical_sell / NULLIF(LAG(dd.typical_sell) OVER (PARTITION BY dd.type_id ORDER BY dd.day), 0)) AS ret
            FROM market_region_stats_daily dd
            WHERE dd.region_id = sqlc.arg(region_id)
              AND dd.day > to_char(sqlc.arg(today)::date - 30, 'YYYY-MM-DD')
              AND dd.typical_sell > 0
              AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR dd.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
        ) x
        GROUP BY x.type_id
    ) v ON v.type_id = r.type_id
    LEFT JOIN hist h ON h.type_id = r.type_id
    WHERE r.region_id = sqlc.arg(region_id)
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR r.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
) t
WHERE s.region_id = sqlc.arg(region_id) AND s.type_id = t.type_id;

-- name: RefreshMarketHubConfirmation :execrows
-- Updates confirmed_hubs across the trade hub regions for the swept
-- types: how many other hub regions show a move of at least 5% in the
-- same direction with recorded trade activity.
UPDATE market_region_stats s
SET confirmed_hubs = CASE
    WHEN COALESCE(s.mover_chg_7v30, s.sell_chg_7d) > 0 THEN
        GREATEST(0, COALESCE(c.up_hubs, 0) - CASE WHEN s.typical_sell > 0 AND s.isk_traded_7d > 0 AND COALESCE(s.mover_chg_7v30, s.sell_chg_7d) >= 5.0 THEN 1 ELSE 0 END)
    WHEN COALESCE(s.mover_chg_7v30, s.sell_chg_7d) < 0 THEN
        GREATEST(0, COALESCE(c.down_hubs, 0) - CASE WHEN s.typical_sell > 0 AND s.isk_traded_7d > 0 AND COALESCE(s.mover_chg_7v30, s.sell_chg_7d) <= -5.0 THEN 1 ELSE 0 END)
    ELSE 0
END
FROM (
    SELECT type_id,
           COUNT(*) FILTER (WHERE typical_sell > 0 AND isk_traded_7d > 0 AND COALESCE(mover_chg_7v30, sell_chg_7d) >= 5.0)::bigint AS up_hubs,
           COUNT(*) FILTER (WHERE typical_sell > 0 AND isk_traded_7d > 0 AND COALESCE(mover_chg_7v30, sell_chg_7d) <= -5.0)::bigint AS down_hubs
    FROM market_region_stats
    WHERE region_id = ANY(sqlc.arg(hub_regions)::bigint[])
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR type_id = ANY(sqlc.arg(type_ids)::bigint[]))
    GROUP BY type_id
) c
WHERE s.type_id = c.type_id
  AND s.region_id = ANY(sqlc.arg(hub_regions)::bigint[]);

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
-- One station's current best sell for each type asked about:
-- the appraisal tool's sell list prices 0.01 under it.
-- name: ListMarketStationBestsByTypes :many
SELECT type_id, best_sell
FROM market_station_stats
WHERE location_id = $1 AND type_id = ANY($2::bigint[])
ORDER BY type_id;

-- Station heat (schema 120): the trailing 12 hour count of times
-- the best price at a station was beaten, per item and side,
-- restated by every sweep of the region. The My Orders page reads
-- the rows for its orders' stations in one bounded read; a pair
-- with no row had no beats in the window.
-- name: ListMarketStationHeatByLocations :many
SELECT location_id, region_id, type_id, is_buy_order, beats_12h, computed_at
FROM market_station_heat
WHERE location_id = ANY($1::bigint[])
ORDER BY location_id, type_id, is_buy_order;
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
      AND (sqlc.arg(min_isk)::double precision <= 0 OR COALESCE(rs.avg_daily_volume, 0) * COALESCE(NULLIF(rs.typical_sell, 0), ss.typical_sell) >= sqlc.arg(min_isk)::double precision)
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
-- sale in the origin. Each endpoint resolves per type: the
-- endpoint region's own row, or -- where the region carries no
-- row for the type -- the type's row on ESI's global market
-- (global_region), which is that type's own book (PLEX trades
-- there, not in any hub). A region row always wins over the
-- global row, and the serving row's own updated_at is what the
-- freshness rule judges.
SELECT type_id, origin_typical_sell, dest_typical_sell, dest_daily_volume, origin_sell_volume
FROM (
    SELECT o.type_id,
           o.typical_sell AS origin_typical_sell,
           d.typical_sell AS dest_typical_sell,
           d.avg_daily_volume AS dest_daily_volume,
           o.sell_volume AS origin_sell_volume,
           (d.typical_sell - o.typical_sell) * LEAST(d.avg_daily_volume, o.sell_volume::double precision) AS profit
    FROM market_region_stats o
    JOIN market_region_stats d
      ON d.type_id = o.type_id
     AND d.region_id IN (sqlc.arg(dest_region), sqlc.arg(global_region))
     AND (d.region_id = sqlc.arg(dest_region)
          OR NOT EXISTS (SELECT 1 FROM market_region_stats dd
                         WHERE dd.region_id = sqlc.arg(dest_region) AND dd.type_id = o.type_id))
    WHERE o.region_id IN (sqlc.arg(origin_region), sqlc.arg(global_region))
      AND (o.region_id = sqlc.arg(origin_region)
           OR NOT EXISTS (SELECT 1 FROM market_region_stats oo
                          WHERE oo.region_id = sqlc.arg(origin_region) AND oo.type_id = o.type_id))
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
-- Endpoint rows resolve exactly as in ListTradefinderRoutes: the
-- endpoint region's own row, else the type's global-market row.
SELECT o.type_id,
       o.typical_sell AS origin_typical_sell,
       d.typical_sell AS dest_typical_sell
FROM market_region_stats o
JOIN market_region_stats d
  ON d.type_id = o.type_id
 AND d.region_id IN (sqlc.arg(dest_region), sqlc.arg(global_region))
 AND (d.region_id = sqlc.arg(dest_region)
      OR NOT EXISTS (SELECT 1 FROM market_region_stats dd
                     WHERE dd.region_id = sqlc.arg(dest_region) AND dd.type_id = o.type_id))
WHERE o.region_id IN (sqlc.arg(origin_region), sqlc.arg(global_region))
  AND (o.region_id = sqlc.arg(origin_region)
       OR NOT EXISTS (SELECT 1 FROM market_region_stats oo
                      WHERE oo.region_id = sqlc.arg(origin_region) AND oo.type_id = o.type_id))
  AND o.typical_sell > 0 AND d.typical_sell > o.typical_sell
  AND (sqlc.arg(include_lowball)::bigint = 1 OR d.typical_sell <= o.typical_sell * 10)
  AND (d.typical_sell - o.typical_sell) / o.typical_sell * 100 >= sqlc.arg(min_margin)
  AND d.avg_daily_volume = 0
  AND o.sell_volume > 0
  AND o.updated_at >= sqlc.arg(cutoff) AND d.updated_at >= sqlc.arg(cutoff)
ORDER BY (d.typical_sell - o.typical_sell) * o.sell_volume::double precision DESC, o.type_id
LIMIT sqlc.arg(row_cap)::bigint;
-- name: RefreshMarketRegionStatAvgVolume :exec
-- Recomputes one region stat's traded-volume, traded-price medians,
-- hold days, and mover-activity figures from its stored history right
-- after a history fetch, so neither the scanner/tradefinder nor
-- Trends/Movers wait for the next region sweep.
WITH hr AS (
    SELECT CASE WHEN MAX(m.date)::date >= (now() AT TIME ZONE 'UTC')::date - 2
                 AND MAX(m.date)::date <= (now() AT TIME ZONE 'UTC')::date
                THEN MAX(m.date)::date
                ELSE (now() AT TIME ZONE 'UTC')::date - 1
           END AS ref_date,
           MAX(m.date)::date AS max_date
    FROM market_history m
    WHERE m.region_id = sqlc.arg(region_id) AND m.type_id = sqlc.arg(type_id)
),
agg AS (
    SELECT COALESCE(AVG(h.volume) FILTER (WHERE h.date >= to_char(hr.max_date - 6, 'YYYY-MM-DD')), 0)::double precision AS avg_daily_volume,
           COALESCE(AVG(h.volume) FILTER (WHERE h.date >= to_char(hr.max_date - 29, 'YYYY-MM-DD')), 0)::double precision AS avg_daily_volume_30d,
           COALESCE(SUM(h.volume::double precision * h.average)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS isk_traded_7d,
           COALESCE(SUM(h.order_count::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_orders_7d,
           COALESCE(SUM(h.volume::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_vol_7d,
           COALESCE(SUM(h.volume::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 30.0 AS avg_vol_30d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY h.average)
               FILTER (WHERE h.average > 0 AND h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_7d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY h.average)
               FILTER (WHERE h.average > 0 AND h.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_30d
    FROM hr
    LEFT JOIN market_history h
      ON h.region_id = sqlc.arg(region_id)
     AND h.type_id = sqlc.arg(type_id)
     AND hr.max_date IS NOT NULL
     AND h.date >= to_char(LEAST(hr.ref_date - 29, hr.max_date - 29), 'YYYY-MM-DD')
),
hold AS (
    SELECT COALESCE(COUNT(*) FILTER (WHERE
               (agg.sell_median_7d > agg.sell_median_30d
                AND h.average >= agg.sell_median_30d + 0.5 * (agg.sell_median_7d - agg.sell_median_30d))
            OR (agg.sell_median_7d < agg.sell_median_30d
                AND h.average <= agg.sell_median_30d + 0.5 * (agg.sell_median_7d - agg.sell_median_30d))
           ), 0)::bigint AS hold_days
    FROM hr
    CROSS JOIN agg
    LEFT JOIN market_history h
      ON h.region_id = sqlc.arg(region_id)
     AND h.type_id = sqlc.arg(type_id)
     AND hr.max_date IS NOT NULL
     AND h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD')
     AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')
     AND h.average > 0
     AND agg.sell_median_7d > 0
     AND agg.sell_median_30d > 0
)
UPDATE market_region_stats s SET
    avg_daily_volume     = agg.avg_daily_volume,
    avg_daily_volume_30d = agg.avg_daily_volume_30d,
    sell_median_7d       = agg.sell_median_7d,
    sell_median_30d      = agg.sell_median_30d,
    mover_chg_7v30       = CASE WHEN agg.sell_median_7d > 0 AND agg.sell_median_30d > 0
                                THEN (agg.sell_median_7d - agg.sell_median_30d) / agg.sell_median_30d * 100.0 END,
    mover_hold_days      = hold.hold_days,
    isk_traded_7d        = agg.isk_traded_7d,
    avg_orders_7d        = agg.avg_orders_7d,
    avg_vol_7d           = agg.avg_vol_7d,
    avg_vol_30d          = agg.avg_vol_30d,
    mover_score          = CASE WHEN agg.sell_median_7d > 0 AND agg.sell_median_30d > 0
                                THEN ABS((agg.sell_median_7d - agg.sell_median_30d) / agg.sell_median_30d * 100.0) * LN(1.0 + GREATEST(agg.isk_traded_7d, 0)) END,
    isk_move_7d          = CASE WHEN agg.sell_median_7d > 0 AND agg.sell_median_30d > 0
                                THEN ABS(agg.sell_median_7d - agg.sell_median_30d) * GREATEST(agg.avg_vol_7d, 0)
                                ELSE 0 END,
    daily_spread_profit  = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                                THEN (s.typical_sell - s.typical_buy) * LEAST(GREATEST(agg.avg_vol_7d, 0), s.sell_volume::double precision, s.buy_volume::double precision)
                                ELSE 0 END
FROM agg, hold
WHERE s.region_id = sqlc.arg(region_id) AND s.type_id = sqlc.arg(type_id);
-- name: RefreshMarketRegionStatAvgVolumes :execrows
-- RefreshMarketRegionStatAvgVolume for several types of one region in
-- one statement, updating traded-volume, traded medians, hold days,
-- and mover-activity metrics for rows whose figures changed.
WITH hr AS (
    SELECT m.type_id,
           CASE WHEN MAX(m.date)::date >= (now() AT TIME ZONE 'UTC')::date - 2
                 AND MAX(m.date)::date <= (now() AT TIME ZONE 'UTC')::date
                THEN MAX(m.date)::date
                ELSE (now() AT TIME ZONE 'UTC')::date - 1
           END AS ref_date,
           MAX(m.date)::date AS max_date
    FROM market_history m
    WHERE m.region_id = sqlc.arg(region_id) AND m.type_id = ANY(sqlc.arg(type_ids)::bigint[])
    GROUP BY m.type_id
),
agg AS (
    SELECT h.type_id,
           COALESCE(AVG(h.volume) FILTER (WHERE h.date >= to_char(hr.max_date - 6, 'YYYY-MM-DD')), 0)::double precision AS avg_daily_volume,
           COALESCE(AVG(h.volume) FILTER (WHERE h.date >= to_char(hr.max_date - 29, 'YYYY-MM-DD')), 0)::double precision AS avg_daily_volume_30d,
           COALESCE(SUM(h.volume::double precision * h.average)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS isk_traded_7d,
           COALESCE(SUM(h.order_count::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_orders_7d,
           COALESCE(SUM(h.volume::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 7.0 AS avg_vol_7d,
           COALESCE(SUM(h.volume::double precision)
               FILTER (WHERE h.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision / 30.0 AS avg_vol_30d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY h.average)
               FILTER (WHERE h.average > 0 AND h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_7d,
           COALESCE(PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY h.average)
               FILTER (WHERE h.average > 0 AND h.date >= to_char(hr.ref_date - 29, 'YYYY-MM-DD') AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')), 0)::double precision AS sell_median_30d
    FROM market_history h
    JOIN hr ON hr.type_id = h.type_id
    WHERE h.region_id = sqlc.arg(region_id)
      AND h.type_id = ANY(sqlc.arg(type_ids)::bigint[])
      AND h.date >= to_char(LEAST(hr.ref_date - 29, hr.max_date - 29), 'YYYY-MM-DD')
    GROUP BY h.type_id
),
hold AS (
    SELECT h.type_id,
           COUNT(*) FILTER (WHERE
               (agg.sell_median_7d > agg.sell_median_30d
                AND h.average >= agg.sell_median_30d + 0.5 * (agg.sell_median_7d - agg.sell_median_30d))
            OR (agg.sell_median_7d < agg.sell_median_30d
                AND h.average <= agg.sell_median_30d + 0.5 * (agg.sell_median_7d - agg.sell_median_30d))
           )::bigint AS hold_days
    FROM market_history h
    JOIN hr ON hr.type_id = h.type_id
    JOIN agg ON agg.type_id = h.type_id
    WHERE h.region_id = sqlc.arg(region_id)
      AND h.type_id = ANY(sqlc.arg(type_ids)::bigint[])
      AND h.date >= to_char(hr.ref_date - 6, 'YYYY-MM-DD')
      AND h.date <= to_char(hr.ref_date, 'YYYY-MM-DD')
      AND h.average > 0
      AND agg.sell_median_7d > 0
      AND agg.sell_median_30d > 0
    GROUP BY h.type_id
)
UPDATE market_region_stats s SET
    avg_daily_volume     = v.avg_daily_volume,
    avg_daily_volume_30d = v.avg_daily_volume_30d,
    sell_median_7d       = v.sell_median_7d,
    sell_median_30d      = v.sell_median_30d,
    mover_chg_7v30       = CASE WHEN v.sell_median_7d > 0 AND v.sell_median_30d > 0
                                THEN (v.sell_median_7d - v.sell_median_30d) / v.sell_median_30d * 100.0 END,
    mover_hold_days      = v.hold_days,
    isk_traded_7d        = v.isk_traded_7d,
    avg_orders_7d        = v.avg_orders_7d,
    avg_vol_7d           = v.avg_vol_7d,
    avg_vol_30d          = v.avg_vol_30d,
    mover_score          = CASE WHEN v.sell_median_7d > 0 AND v.sell_median_30d > 0
                                THEN ABS((v.sell_median_7d - v.sell_median_30d) / v.sell_median_30d * 100.0) * LN(1.0 + GREATEST(v.isk_traded_7d, 0)) END,
    isk_move_7d          = CASE WHEN v.sell_median_7d > 0 AND v.sell_median_30d > 0
                                THEN ABS(v.sell_median_7d - v.sell_median_30d) * GREATEST(v.avg_vol_7d, 0)
                                ELSE 0 END,
    daily_spread_profit  = CASE WHEN s.typical_buy > 0 AND s.typical_sell > s.typical_buy
                                THEN (s.typical_sell - s.typical_buy) * LEAST(GREATEST(v.avg_vol_7d, 0), s.sell_volume::double precision, s.buy_volume::double precision)
                                ELSE 0 END
FROM (
    SELECT a.type_id, a.avg_daily_volume, a.avg_daily_volume_30d,
           a.sell_median_7d, a.sell_median_30d,
           a.isk_traded_7d, a.avg_orders_7d, a.avg_vol_7d, a.avg_vol_30d,
           COALESCE(hd.hold_days, 0)::bigint AS hold_days
    FROM agg a
    LEFT JOIN hold hd ON hd.type_id = a.type_id
) v
WHERE s.region_id = sqlc.arg(region_id)
  AND s.type_id = v.type_id
  AND (
      s.avg_daily_volume <> v.avg_daily_volume
      OR s.isk_traded_7d <> v.isk_traded_7d
      OR s.avg_orders_7d <> v.avg_orders_7d
      OR s.sell_median_7d <> v.sell_median_7d
      OR s.sell_median_30d <> v.sell_median_30d
      OR s.mover_hold_days <> v.hold_days
  );
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
-- name: ListOrderLifecycleStates :many
-- The ledger rows of many (character, order) pairs in one read: what
-- the order-health pass needs of each before it upserts them.
SELECT l.character_id, l.order_id, l.closed_at, l.outbid_events, l.beaten_now,
       l.beating_price, l.beating_volume, l.suggested_price, l.unit_cost, l.margin_pct, l.tax_rate
FROM order_lifecycle l
JOIN (SELECT unnest(sqlc.arg(character_ids)::bigint[]) AS character_id,
             unnest(sqlc.arg(order_ids)::bigint[]) AS order_id) k
  ON l.character_id = k.character_id AND l.order_id = k.order_id;
-- name: UpsertOrderLifecycles :exec
-- UpsertOrderLifecycle for every open order of a pass in one
-- statement. The (character, order) pairs must be distinct.
INSERT INTO order_lifecycle (character_id, order_id, type_id, location_id, region_id, is_buy_order, listed_price, volume_total, volume_remain_last, first_seen_at, last_seen_at, closed_at, close_kind, outbid_events, beaten_now)
SELECT unnest(sqlc.arg(character_ids)::bigint[]), unnest(sqlc.arg(order_ids)::bigint[]),
    unnest(sqlc.arg(type_ids)::bigint[]), unnest(sqlc.arg(location_ids)::bigint[]),
    unnest(sqlc.arg(region_ids)::bigint[]), unnest(sqlc.arg(is_buy_orders)::bigint[]),
    unnest(sqlc.arg(listed_prices)::float8[]), unnest(sqlc.arg(volume_totals)::bigint[]),
    unnest(sqlc.arg(volume_remains)::bigint[]),
    sqlc.arg(seen_at)::timestamptz, sqlc.arg(seen_at)::timestamptz, NULL, '', 0, 0
ON CONFLICT (character_id, order_id) DO UPDATE SET
    type_id            = excluded.type_id,
    location_id        = excluded.location_id,
    region_id          = excluded.region_id,
    is_buy_order       = excluded.is_buy_order,
    listed_price       = excluded.listed_price,
    volume_total       = excluded.volume_total,
    volume_remain_last = excluded.volume_remain_last,
    last_seen_at       = excluded.last_seen_at;
-- name: CloseOrderLifecycle :exec
UPDATE order_lifecycle
SET closed_at = sqlc.arg(closed_at)::timestamptz, close_kind = sqlc.arg(close_kind), beaten_now = 0
WHERE character_id = sqlc.arg(character_id) AND order_id = sqlc.arg(order_id) AND closed_at IS NULL;
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
-- name: GetOrderLifecycleSummary :one
-- One character's lifecycle summary strip in one aggregate: orders
-- tracked, outbid events, closed orders filled and ended, and the
-- median time from first seen to filled (seconds; averages the two
-- middle values on an even count).
SELECT
    COUNT(*)::bigint AS tracked,
    COALESCE(SUM(outbid_events), 0)::bigint AS outbid_events,
    (COUNT(*) FILTER (WHERE closed_at IS NOT NULL AND close_kind = 'filled'))::bigint AS filled,
    (COUNT(*) FILTER (WHERE closed_at IS NOT NULL AND close_kind <> 'filled'))::bigint AS ended,
    COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM closed_at - first_seen_at))
        FILTER (WHERE closed_at IS NOT NULL AND close_kind = 'filled' AND closed_at >= first_seen_at), -1)::float8 AS median_fill_seconds
FROM order_lifecycle
WHERE character_id = $1;
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
-- Stored book (schema 050): the top orders per side a completed
-- sweep keeps, so the Market page can show an order book to
-- visitors who never fetch (anonymous readers among them). The
-- sweep's store replaces the rows of every item it writes,
-- exactly like the region stats distilled beside them; figures
-- over the whole book (best, typical, depth) stay on
-- market_region_stats, these rows are only the displayed top.
-- ---------------------------------------------------------------------
-- name: DeleteMarketBookOrdersByRegion :exec
DELETE FROM market_book_orders
WHERE region_id = $1;
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

-- ---------------------------------------------------------------------
-- Station leaderboard (schema_pg 002): per-station open-order
-- counts and open ISK value per side, distilled by the same
-- whole-region sweeps as the stats above. A completed sweep
-- replaces the region's leaderboard rows inside the sweep
-- transaction: delete the region, then upsert the fresh measures
-- station by station. The sweep sums them from the book it read,
-- grouped only by place: the open value needs price times
-- remaining volume per order, which the type-grain aggregates
-- cannot reconstruct. The leaderboard page
-- reads only these rows. A region argument of 0 means every
-- region.
-- ---------------------------------------------------------------------
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
-- name: ListMarketRegionStatsDailyRecent :many
-- One (region, type)'s stat pack history from a start day on,
-- oldest first: the item page's book trend chart. The caller
-- bounds the window: at most row_limit rows.
SELECT day, typical_sell, typical_buy, best_sell, best_buy, sell_orders, buy_orders, sell_volume, buy_volume
FROM market_region_stats_daily
WHERE region_id = sqlc.arg(region_id) AND type_id = sqlc.arg(type_id) AND day >= sqlc.arg(since)
ORDER BY day
LIMIT sqlc.arg(row_limit);
-- name: DeleteMarketMoversRankedByRegion :exec
DELETE FROM market_movers_ranked WHERE region_id = $1;

-- name: InsertMarketMoversRankedForRegion :execrows
-- Precomputes ranked mover rows for one region across all sort modes,
-- directions (including combined 'both' for the tall Market Movers
-- widget), and periods (0 for volatile). Run inside the region sweep
-- transaction and after history refreshes so renders read with LIMIT only.
WITH candidates AS (
    SELECT s.type_id, s.isk_traded_7d, s.avg_orders_7d,
           b.sort_by, b.mode, b.period, b.score
    FROM market_region_stats s
    CROSS JOIN LATERAL (
        SELECT 'confirmed'::text AS sort_by, 'rising'::text AS mode, 1::integer AS period, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_1d > 0 THEN ABS(s.sell_chg_1d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END AS score
        UNION ALL SELECT 'confirmed', 'falling',  1,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_1d < 0 THEN ABS(s.sell_chg_1d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'rising',   7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND s.mover_chg_7v30 > 0 THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'falling',  7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND s.mover_chg_7v30 < 0 THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'both',     7,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.mover_hold_days >= 4 AND (s.mover_chg_7v30 > 0 OR s.mover_chg_7v30 < 0) THEN s.mover_score END
        UNION ALL SELECT 'confirmed', 'rising',   30, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_30d > 0 THEN ABS(s.sell_chg_30d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'falling',  30, CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.sell_chg_30d < 0 THEN ABS(s.sell_chg_30d) * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'confirmed', 'volatile', 0,  CASE WHEN s.avg_vol_7d > 0 AND s.avg_vol_7d >= s.avg_vol_30d AND s.volatility_30d > 0 THEN s.volatility_30d * LN(1.0 + GREATEST(s.isk_traded_7d, 0)) END
        UNION ALL SELECT 'isk_move', 'rising',   1,  CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_1d > 0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_1d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'falling',  1,  CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_1d < 0 AND s.sell_chg_1d > -100.0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_1d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'rising',   7,  CASE WHEN s.avg_vol_7d > 0 AND s.mover_chg_7v30 > 0 AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'falling',  7,  CASE WHEN s.avg_vol_7d > 0 AND s.mover_chg_7v30 < 0 AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'both',     7,  CASE WHEN s.avg_vol_7d > 0 AND (s.mover_chg_7v30 > 0 OR s.mover_chg_7v30 < 0) AND s.isk_move_7d > 0 THEN s.isk_move_7d END
        UNION ALL SELECT 'isk_move', 'rising',   30, CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_30d > 0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_30d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'falling',  30, CASE WHEN s.avg_vol_7d > 0 AND s.sell_chg_30d < 0 AND s.sell_chg_30d > -100.0 THEN ABS(s.typical_sell - s.typical_sell / (1.0 + s.sell_chg_30d / 100.0)) * s.avg_vol_7d END
        UNION ALL SELECT 'isk_move', 'volatile', 0,  CASE WHEN s.avg_vol_7d > 0 AND s.volatility_30d > 0 THEN s.typical_sell * s.avg_vol_7d * (s.volatility_30d / 100.0) END
        UNION ALL SELECT 'spread_profit', 'rising',   1,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_1d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  1,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_1d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'rising',   7,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_7d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  7,  CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_7d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'both',     7,  CASE WHEN s.daily_spread_profit > 0 AND (s.sell_chg_7d > 0 OR s.sell_chg_7d < 0) THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'rising',   30, CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_30d > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'falling',  30, CASE WHEN s.daily_spread_profit > 0 AND s.sell_chg_30d < 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'spread_profit', 'volatile', 0,  CASE WHEN s.daily_spread_profit > 0 THEN s.daily_spread_profit END
        UNION ALL SELECT 'raw_pct', 'rising',   1,  CASE WHEN s.sell_chg_1d > 0 THEN s.sell_chg_1d END
        UNION ALL SELECT 'raw_pct', 'falling',  1,  CASE WHEN s.sell_chg_1d < 0 THEN -s.sell_chg_1d END
        UNION ALL SELECT 'raw_pct', 'rising',   7,  CASE WHEN s.sell_chg_7d > 0 THEN s.sell_chg_7d END
        UNION ALL SELECT 'raw_pct', 'falling',  7,  CASE WHEN s.sell_chg_7d < 0 THEN -s.sell_chg_7d END
        UNION ALL SELECT 'raw_pct', 'both',     7,  CASE WHEN s.sell_chg_7d > 0 OR s.sell_chg_7d < 0 THEN ABS(s.sell_chg_7d) END
        UNION ALL SELECT 'raw_pct', 'rising',   30, CASE WHEN s.sell_chg_30d > 0 THEN s.sell_chg_30d END
        UNION ALL SELECT 'raw_pct', 'falling',  30, CASE WHEN s.sell_chg_30d < 0 THEN -s.sell_chg_30d END
        UNION ALL SELECT 'raw_pct', 'volatile', 0,  CASE WHEN s.volatility_30d > 0 THEN s.volatility_30d END
    ) b
    WHERE s.region_id = sqlc.arg(region_id)
      AND s.typical_sell > 0
      AND b.score IS NOT NULL
      AND b.score > 0
),
ranked AS (
    SELECT sort_by, mode, period, type_id, score, isk_traded_7d, avg_orders_7d,
           ROW_NUMBER() OVER (PARTITION BY sort_by, mode, period ORDER BY score DESC, type_id ASC)::integer AS rank
    FROM candidates
)
INSERT INTO market_movers_ranked (region_id, sort_by, mode, period, rank, type_id, score, isk_traded_7d, avg_orders_7d, updated_at)
SELECT sqlc.arg(region_id), sort_by, mode, period, rank, type_id, score, isk_traded_7d, avg_orders_7d, sqlc.arg(now)
FROM ranked
WHERE sort_by <> 'raw_pct' OR rank <= 100 OR isk_traded_7d > 0;

-- name: ListRankedMarketMovers :many
-- Reads pre-ranked movers for the Trends screen and Market Movers widget
-- directly from market_movers_ranked in primary-key rank order with LIMIT.
SELECT m.rank, m.score,
       s.type_id, s.typical_sell, s.typical_buy, s.sell_orders, s.avg_daily_volume_30d,
       s.sell_avg_7d, s.sell_median_7d, s.sell_median_30d, s.mover_chg_7v30, s.mover_hold_days,
       s.isk_traded_7d, s.avg_orders_7d, s.avg_vol_7d, s.avg_vol_30d,
       s.mover_score, s.isk_move_7d, s.daily_spread_profit, s.spread_pct, s.spread_widened, s.confirmed_hubs,
       s.sell_chg_1d, s.sell_chg_7d, s.sell_chg_30d, s.volatility_30d, s.history_days, s.updated_at,
       s.sell_spark_30d, s.sell_low_30d, s.sell_high_30d, s.sell_mean_30d
FROM market_movers_ranked m
JOIN market_region_stats s
  ON s.region_id = m.region_id AND s.type_id = m.type_id
WHERE m.region_id = sqlc.arg(region_id)
  AND m.sort_by = sqlc.arg(sort_by)::text
  AND m.mode = sqlc.arg(mode)::text
  AND m.period = sqlc.arg(period)::int
  AND (sqlc.arg(sort_by)::text = 'raw_pct' OR (
      m.isk_traded_7d >= sqlc.arg(min_isk)::double precision
      AND m.avg_orders_7d >= sqlc.arg(min_trades)::double precision
  ))
ORDER BY m.rank
LIMIT sqlc.arg(row_cap)::bigint;

-- name: GetMarketTrendStamp :one
-- Checks whether pre-ranked movers exist for a region and when they were
-- last written, in one bounded index probe.
SELECT updated_at FROM market_movers_ranked WHERE region_id = $1 LIMIT 1;
-- name: ListSDERegionNames :many
SELECT region_id, name FROM sde_regions WHERE region_id = ANY(sqlc.arg(region_ids)::bigint[]);
-- name: ListMarketBrowseTypes :many
-- The market browser's item list: every marketable type under a
-- market group (its whole subtree; 0 = every group), joined to the
-- region's stat pack row, filtered by name and traded volume, sorted
-- and paged in SQL. A type with no stat row lists with no figures
-- and sorts after priced ones. A type the region does not carry
-- reads the global market's row (region 19000001: PLEX trades there
-- and nowhere else).
WITH RECURSIVE tree AS (
    SELECT g.market_group_id FROM sde_market_groups g
    WHERE (sqlc.arg(group_id)::bigint = 0 AND g.parent_group_id = 0) OR g.market_group_id = sqlc.arg(group_id)::bigint
    UNION ALL
    SELECT c.market_group_id FROM sde_market_groups c JOIN tree t ON c.parent_group_id = t.market_group_id
)
SELECT t.type_id, t.name,
       COALESCE(rs.typical_sell, gs.typical_sell, 0)::float8 AS typical_sell,
       COALESCE(rs.typical_buy, gs.typical_buy, 0)::float8 AS typical_buy,
       COALESCE(rs.sell_chg_7d, gs.sell_chg_7d) AS sell_chg_7d,
       COALESCE(rs.volatility_30d, gs.volatility_30d) AS volatility_30d,
       COALESCE(rs.avg_daily_volume_30d, gs.avg_daily_volume_30d, 0)::float8 AS traded_per_day,
       COALESCE(rs.sell_orders, gs.sell_orders, 0)::bigint AS sell_orders
FROM sde_types t
LEFT JOIN market_region_stats rs ON rs.region_id = sqlc.arg(region_id)::bigint AND rs.type_id = t.type_id
LEFT JOIN market_region_stats gs ON gs.region_id = 19000001 AND gs.type_id = t.type_id AND rs.type_id IS NULL
WHERE t.published = 1 AND t.market_group_id IN (SELECT market_group_id FROM tree)
  AND (sqlc.arg(name_like)::text = '' OR t.name ILIKE sqlc.arg(name_like)::text)
  AND COALESCE(rs.avg_daily_volume_30d, gs.avg_daily_volume_30d, 0) >= sqlc.arg(min_volume)::float8
ORDER BY
  -- Best match (a name search's default): the exact name, then names
  -- that start with the words, then a word in the name that starts
  -- with them, then any name holding them; blueprints after their
  -- items unless the search asks for one; then what trades the most
  -- ISK a day.
  CASE WHEN sqlc.arg(sort)::text = 'match' THEN
    CASE WHEN lower(t.name) = sqlc.arg(match_name)::text THEN 0
         WHEN lower(t.name) LIKE sqlc.arg(match_prefix)::text THEN 1
         WHEN strpos(' ' || regexp_replace(lower(t.name), '[^a-z0-9]+', ' ', 'g'), ' ' || sqlc.arg(match_words)::text) > 0 THEN 2
         ELSE 3 END END,
  CASE WHEN sqlc.arg(sort)::text = 'match' AND strpos(sqlc.arg(match_name)::text, 'blueprint') = 0 AND lower(t.name) LIKE '% blueprint' THEN 1 END NULLS FIRST,
  CASE WHEN sqlc.arg(sort)::text = 'match' THEN NULLIF(COALESCE(rs.avg_daily_volume_30d, gs.avg_daily_volume_30d, 0) * COALESCE(rs.typical_sell, gs.typical_sell, 0), 0) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'name' THEN t.name END,
  CASE WHEN sqlc.arg(sort)::text = 'price' THEN COALESCE(rs.typical_sell, gs.typical_sell) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'cheap' THEN NULLIF(COALESCE(rs.typical_sell, gs.typical_sell), 0) END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'buy' THEN COALESCE(rs.typical_buy, gs.typical_buy) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'buycheap' THEN NULLIF(COALESCE(rs.typical_buy, gs.typical_buy), 0) END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'rising' THEN COALESCE(rs.sell_chg_7d, gs.sell_chg_7d) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'falling' THEN COALESCE(rs.sell_chg_7d, gs.sell_chg_7d) END ASC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'volume' THEN COALESCE(rs.avg_daily_volume_30d, gs.avg_daily_volume_30d) END DESC NULLS LAST,
  CASE WHEN sqlc.arg(sort)::text = 'volatile' THEN COALESCE(rs.volatility_30d, gs.volatility_30d) END DESC NULLS LAST,
  t.name, t.type_id
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;
-- name: CountMarketBrowseTypes :one
WITH RECURSIVE tree AS (
    SELECT g.market_group_id FROM sde_market_groups g
    WHERE (sqlc.arg(group_id)::bigint = 0 AND g.parent_group_id = 0) OR g.market_group_id = sqlc.arg(group_id)::bigint
    UNION ALL
    SELECT c.market_group_id FROM sde_market_groups c JOIN tree t ON c.parent_group_id = t.market_group_id
)
SELECT COUNT(*) FROM sde_types t
LEFT JOIN market_region_stats rs ON rs.region_id = sqlc.arg(region_id)::bigint AND rs.type_id = t.type_id
LEFT JOIN market_region_stats gs ON gs.region_id = 19000001 AND gs.type_id = t.type_id AND rs.type_id IS NULL
WHERE t.published = 1 AND t.market_group_id IN (SELECT market_group_id FROM tree)
  AND (sqlc.arg(name_like)::text = '' OR t.name ILIKE sqlc.arg(name_like)::text)
  AND COALESCE(rs.avg_daily_volume_30d, gs.avg_daily_volume_30d, 0) >= sqlc.arg(min_volume)::float8;
-- name: UpdateOrderLifecycleBook :exec
-- The order-health pass's verdict on one open order's station book
-- (schema 105): beaten state and count, when it was beaten, what
-- beat it, the suggested reprice, and the margin at that price.
UPDATE order_lifecycle SET
    beaten_at = CASE
        WHEN sqlc.arg(beaten)::bigint = 1 AND (beaten_now = 0 OR beaten_at IS NULL) THEN sqlc.arg(checked_at)::timestamptz
        WHEN sqlc.arg(beaten)::bigint = 0 THEN NULL
        ELSE beaten_at END,
    beaten_now      = sqlc.arg(beaten)::bigint,
    outbid_events   = sqlc.arg(outbid_events)::bigint,
    beating_price   = sqlc.arg(beating_price)::float8,
    beating_volume  = sqlc.arg(beating_volume)::bigint,
    suggested_price = sqlc.arg(suggested_price)::float8,
    unit_cost       = sqlc.arg(unit_cost)::float8,
    margin_pct      = sqlc.narg(margin_pct)::float8,
    tax_rate        = sqlc.arg(tax_rate)::float8,
    beaten_checked  = sqlc.arg(checked_at)::timestamptz
WHERE character_id = sqlc.arg(character_id) AND order_id = sqlc.arg(order_id) AND closed_at IS NULL;
-- name: TouchOrderLifecycleChecked :exec
-- The book pass re-checked this order and its verdict stands: only
-- the freshness stamp moves, not the verdict columns.
UPDATE order_lifecycle SET beaten_checked = sqlc.arg(checked_at)::timestamptz
WHERE character_id = sqlc.arg(character_id) AND order_id = sqlc.arg(order_id) AND closed_at IS NULL;

-- name: ListBeatenOrders :many
-- The account's open orders beaten at their own station, most
-- recently beaten first: the My Orders page's beaten section.
SELECT l.character_id, c.name AS character_name, l.order_id, l.type_id, l.location_id, l.region_id,
       l.is_buy_order, l.listed_price, l.volume_total, l.volume_remain_last, l.beaten_at, l.outbid_events,
       l.beating_price, l.beating_volume, l.suggested_price, l.unit_cost, l.margin_pct, l.tax_rate, l.beaten_checked
FROM order_lifecycle l
JOIN characters c ON c.character_id = l.character_id
WHERE c.user_id = sqlc.arg(user_id) AND l.beaten_now = 1 AND l.closed_at IS NULL
  AND (sqlc.arg(character_id)::bigint = 0 OR l.character_id = sqlc.arg(character_id)::bigint)
ORDER BY l.beaten_at DESC NULLS LAST, l.order_id
LIMIT 100;
-- name: DetectMarketChanges :execrows
-- The change feed (schema 106) for one region, inside its sweep
-- transaction after the trend figures:
--   * price_up / price_down compare the 7-day traded median against
--     the 30-day traded median from market_history (sell_median_7d vs
--     sell_median_30d, mover_chg_7v30), gated by hub-scaled min_move
--     and 3x daily volatility.
--   * supply_down and spread_wide compare current book supply and
--     spread against the trailing 7-day daily sweep median baseline.
--   * Every kind enforces the shared min_isk (7d average ISK traded
--     per day) and min_trades (7d average trades per day) floors.
-- type_ids narrows it to the items a sweep wrote; empty means all.
INSERT INTO market_changes (region_id, type_id, day, kind, before, after, pct, detected_at)
SELECT sqlc.arg(region_id)::bigint, c.type_id, sqlc.arg(day)::text, c.kind, c.before, c.after, c.pct, sqlc.arg(now)::timestamptz
FROM (
    SELECT s.type_id, s.volatility_30d, k.kind, k.before, k.after, k.pct
    FROM market_region_stats s
    LEFT JOIN LATERAL (
        SELECT PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY dd.sell_volume)
                   FILTER (WHERE dd.sell_volume > 0)::double precision AS base_supply,
               PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY (dd.typical_sell - dd.typical_buy) / dd.typical_buy * 100.0)
                   FILTER (WHERE dd.typical_buy > 0 AND dd.typical_sell > dd.typical_buy)::double precision AS base_spread
        FROM market_region_stats_daily dd
        WHERE dd.region_id = s.region_id AND dd.type_id = s.type_id
          AND dd.day < sqlc.arg(day)::text
          AND dd.day >= to_char(sqlc.arg(day)::date - 7, 'YYYY-MM-DD')
    ) d ON true
    CROSS JOIN LATERAL (VALUES
        (CASE WHEN s.mover_chg_7v30 > 0 THEN 'price_up' ELSE 'price_down' END,
         CASE WHEN s.sell_median_30d > 0 AND s.sell_median_7d > 0 THEN s.sell_median_30d END,
         CASE WHEN s.sell_median_30d > 0 AND s.sell_median_7d > 0 THEN s.sell_median_7d END,
         s.mover_chg_7v30),
        ('supply_down', d.base_supply, s.sell_volume::float8,
         CASE WHEN d.base_supply > 0 THEN (s.sell_volume::float8 - d.base_supply) / d.base_supply * 100.0 END),
        ('spread_wide',
         d.base_spread,
         s.spread_pct,
         CASE WHEN s.spread_pct IS NOT NULL AND d.base_spread IS NOT NULL THEN s.spread_pct - d.base_spread END)
    ) AS k (kind, before, after, pct)
    WHERE s.region_id = sqlc.arg(region_id)::bigint
      AND (COALESCE(cardinality(sqlc.arg(type_ids)::bigint[]), 0) = 0 OR s.type_id = ANY(sqlc.arg(type_ids)::bigint[]))
      AND s.isk_traded_7d >= sqlc.arg(min_isk)::double precision
      AND s.avg_orders_7d >= sqlc.arg(min_trades)::double precision
      AND s.typical_sell > 0
) c
WHERE c.before IS NOT NULL AND c.after IS NOT NULL AND c.pct IS NOT NULL AND (
       (c.kind IN ('price_up', 'price_down') AND ABS(c.pct) >= GREATEST(sqlc.arg(min_move)::float8,
            3.0 * COALESCE(c.volatility_30d, 0)))
    OR (c.kind = 'supply_down' AND c.before >= 10 AND c.pct <= -50)
    OR (c.kind = 'spread_wide' AND c.before > 0 AND c.after >= 10 AND c.after >= 2 * c.before)
)
ON CONFLICT (region_id, type_id, day, kind) DO UPDATE SET
    before = excluded.before, after = excluded.after, pct = excluded.pct;
-- name: PruneMarketChanges :exec
DELETE FROM market_changes WHERE region_id = $1 AND day < sqlc.arg(before_day)::text;
-- name: ListMarketChangesForRegion :many
-- A region's change feed from a day on, with the active ISK traded/day
-- and trades/day floors applied in SQL against market_region_stats.
-- Shared by the Trends change feed and the public market changes API.
SELECT c.type_id, c.day, c.kind, c.before, c.after, c.pct, c.detected_at,
       COALESCE(s.isk_traded_7d, 0)::double precision AS isk_traded_7d,
       COALESCE(s.avg_orders_7d, 0)::double precision AS avg_orders_7d,
       COALESCE(s.confirmed_hubs, 0)::bigint AS confirmed_hubs,
       COALESCE(s.spread_widened, false)::boolean AS spread_widened
FROM market_changes c
LEFT JOIN market_region_stats s
  ON s.region_id = c.region_id AND s.type_id = c.type_id
WHERE c.region_id = sqlc.arg(region_id)
  AND c.day >= sqlc.arg(since)::text
  AND (sqlc.arg(kind)::text = '' OR c.kind = sqlc.arg(kind)::text)
  AND (sqlc.arg(min_isk)::double precision <= 0 OR COALESCE(s.isk_traded_7d, 0) >= sqlc.arg(min_isk)::double precision)
  AND (sqlc.arg(min_trades)::double precision <= 0 OR COALESCE(s.avg_orders_7d, 0) >= sqlc.arg(min_trades)::double precision)
ORDER BY c.day DESC, ABS(c.pct) DESC, c.type_id
LIMIT 100;
-- name: ListMarketChangesForType :many
-- One type's recent changes in a region: the item page.
SELECT day, kind, before, after, pct, detected_at
FROM market_changes
WHERE region_id = $1 AND type_id = $2
ORDER BY day DESC, kind
LIMIT 20;
-- name: ListMarketChangesForWatchlist :many
-- Today's changes on a user's watched items (each in its watched
-- region): the notification pass.
SELECT m.type_id, m.region_id, m.day, m.kind, m.before, m.after, m.pct
FROM market_changes m
JOIN market_watchlist w ON w.type_id = m.type_id AND w.region_id = m.region_id
WHERE w.user_id = sqlc.arg(user_id) AND m.day >= sqlc.arg(since)::text
ORDER BY m.day DESC, m.type_id
LIMIT 100;
-- name: GetMarketStatChange7 :one
-- A watched item's stored traded-median move (mover_chg_7v30) and
-- 7- and 30-day typical-sell changes from the stat pack, with when
-- the row was written.
SELECT mover_chg_7v30, sell_chg_7d, sell_chg_30d, typical_sell, updated_at FROM market_region_stats
WHERE region_id = $1 AND type_id = $2;
