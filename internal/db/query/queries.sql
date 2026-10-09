-- name: CreateUser :one
INSERT INTO users (created_at)
VALUES (now())
RETURNING *;
-- name: GetUser :one
SELECT * FROM users
WHERE id = $1;
-- name: UpsertCharacter :one
INSERT INTO characters (
    character_id, user_id, name,
    access_token, refresh_token, token_expiry,
    scopes, cached_until, owner_hash, link_state, link_state_at,
    updated_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6,
    $7, $8, $9, $10, $11,
    now()
)
ON CONFLICT (character_id) DO UPDATE SET
    user_id       = excluded.user_id,
    name          = excluded.name,
    access_token  = excluded.access_token,
    refresh_token = excluded.refresh_token,
    token_expiry  = excluded.token_expiry,
    scopes        = excluded.scopes,
    cached_until  = excluded.cached_until,
    owner_hash    = excluded.owner_hash,
    link_state    = excluded.link_state,
    link_state_at = excluded.link_state_at,
    updated_at    = excluded.updated_at
RETURNING *;
-- name: GetCharacter :one
SELECT * FROM characters
WHERE character_id = $1;
-- name: ListCharactersByUser :many
SELECT * FROM characters
WHERE user_id = $1
ORDER BY name;
-- name: DeleteCharacter :exec
DELETE FROM characters
WHERE character_id = $1 AND user_id = $2;
-- name: SetCharacterTags :exec
UPDATE characters
SET tags = $1, updated_at = now()
WHERE character_id = $2 AND user_id = $3;
-- name: SetCharacterLinkState :exec
UPDATE characters
SET link_state = $1, link_state_at = $2, updated_at = now()
WHERE character_id = $3;
-- name: ListUsers :many
SELECT * FROM users
ORDER BY id;
-- name: ListAllCharacters :many
SELECT * FROM characters
ORDER BY user_id, name;
-- name: UpdateCharacterTokens :exec
UPDATE characters
SET access_token = $1, refresh_token = $2, token_expiry = $3, updated_at = now()
WHERE character_id = $4;
-- name: GetSnapshot :one
SELECT * FROM character_snapshots
WHERE character_id = $1 AND kind = $2;
-- name: UpsertSnapshot :exec
INSERT INTO character_snapshots (character_id, kind, payload, fetched_at, cached_until, etag)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (character_id, kind) DO UPDATE SET
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at,
    cached_until = excluded.cached_until,
    etag         = excluded.etag;
-- name: GetSnapshotETag :one
-- The ETag a snapshot was stored with ('' when it has none), read
-- without its payload: all a conditional refresh needs to send.
SELECT etag FROM character_snapshots
WHERE character_id = $1 AND kind = $2;
-- name: TouchSnapshot :execrows
-- ESI answered "not modified": the stored payload is still current,
-- so only the bookkeeping beside it moves.
UPDATE character_snapshots
SET fetched_at = $1, cached_until = $2
WHERE character_id = $3 AND kind = $4;
-- name: ListSnapshotsByCharacter :many
SELECT * FROM character_snapshots
WHERE character_id = $1
ORDER BY kind;
-- name: ListSnapshotMetaByCharacter :many
-- A character's stored snapshots without their payloads: which kinds
-- exist and how fresh each is. The payloads (a whole asset list, a
-- mailbox) are by far the bulk of the table, and the readers that
-- only order, count or check freshness have no use for them.
SELECT kind, fetched_at, cached_until FROM character_snapshots
WHERE character_id = $1
ORDER BY kind;
-- name: ListSnapshotMetaForCharacters :many
-- Snapshot freshness for many characters at once: the worker's
-- overdue ordering over one query instead of one per character.
SELECT character_id, kind, fetched_at, cached_until FROM character_snapshots
WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[])
ORDER BY character_id, kind;
-- name: ListSnapshotsByKind :many
SELECT * FROM character_snapshots
WHERE kind = $1
ORDER BY character_id;
-- name: GetKillmailDetail :one
SELECT * FROM killmail_details
WHERE killmail_id = $1;
-- name: ListKillmailDetailIDsByCharacter :many
SELECT killmail_id FROM killmail_details
WHERE character_id = $1
ORDER BY killmail_id;
-- name: ListKillmailDetailsByCharacter :many
SELECT * FROM killmail_details
WHERE character_id = $1
ORDER BY killmail_id;
-- name: ListRecentKillmailDetails :many
SELECT payload FROM killmail_details
ORDER BY fetched_at DESC
LIMIT sqlc.arg(row_limit)::bigint;
-- name: ListLiquidCoreTypes :many
SELECT mh.type_id, SUM(mh.volume * mh.average)::bigint AS isk_velocity
FROM market_history mh
WHERE mh.region_id = sqlc.arg(region_id)
  AND mh.date >= (SELECT to_char(MAX(mh2.date)::date - INTERVAL '7 days', 'YYYY-MM-DD') FROM market_history mh2 WHERE mh2.region_id = sqlc.arg(region_id))
GROUP BY mh.type_id
ORDER BY isk_velocity DESC
LIMIT sqlc.arg(core_limit)::bigint;
-- name: UpsertKillmailDetail :exec
INSERT INTO killmail_details (killmail_id, character_id, hash, payload, fetched_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (killmail_id) DO UPDATE SET
    character_id = excluded.character_id,
    hash         = excluded.hash,
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at;

-- ---------------------------------------------------------------------
-- Module sweep, cluster 3 (schema 006): contract detail store. The
-- worker warms contract item lists from the contracts snapshot;
-- pages only read here.
-- ---------------------------------------------------------------------
-- name: GetContractDetail :one
SELECT * FROM contract_details
WHERE contract_id = $1;
-- name: ListContractDetailIDsByCharacter :many
SELECT contract_id FROM contract_details
WHERE character_id = $1
ORDER BY contract_id;
-- name: UpsertContractDetail :exec
INSERT INTO contract_details (contract_id, character_id, payload, fetched_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (contract_id) DO UPDATE SET
    character_id = excluded.character_id,
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at;

-- ---------------------------------------------------------------------
-- Module sweep, cluster 2 (schema 005): corporation support.
-- Fetch-outcome log (role-missing state), character-to-corporation
-- map, and player-given item names.
-- ---------------------------------------------------------------------
-- name: GetSnapshotFetchState :one
SELECT * FROM snapshot_fetch_state
WHERE character_id = $1 AND kind = $2;
-- name: ListSnapshotFetchStatesByCharacter :many
SELECT * FROM snapshot_fetch_state
WHERE character_id = $1
ORDER BY kind;
-- name: UpsertSnapshotFetchState :exec
INSERT INTO snapshot_fetch_state (character_id, kind, state, detail, attempted_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (character_id, kind) DO UPDATE SET
    state        = excluded.state,
    detail       = excluded.detail,
    attempted_at = excluded.attempted_at;
-- name: GetCharacterCorporation :one
SELECT * FROM character_corporations
WHERE character_id = $1;
-- name: UpsertCharacterCorporation :exec
INSERT INTO character_corporations (character_id, corporation_id, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (character_id) DO UPDATE SET
    corporation_id = excluded.corporation_id,
    updated_at     = excluded.updated_at;
-- name: ListAllCorporationIDs :many
SELECT DISTINCT corporation_id FROM character_corporations
ORDER BY corporation_id;
-- name: ListCorporationIDsByUser :many
-- Every corporation the given user's characters belong to,
-- from the worker-maintained character -> corporation map.
-- Scoped to one user: per-user data stays siloed, so the
-- wars page never flags another user's corporation as yours.
SELECT DISTINCT cc.corporation_id
FROM character_corporations cc
JOIN characters c ON c.character_id = cc.character_id
WHERE c.user_id = $1
ORDER BY cc.corporation_id;

-- ---------------------------------------------------------------------
-- Module sweep, cluster 4 (schema 007): intel public-data store.
-- Global snapshots are the public-data counterpart of
-- character_snapshots; war details mirror killmail_details.
-- ---------------------------------------------------------------------
-- name: GetGlobalSnapshot :one
SELECT * FROM global_snapshots
WHERE kind = $1;
-- name: UpsertGlobalSnapshot :exec
INSERT INTO global_snapshots (kind, payload, fetched_at, cached_until, etag)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (kind) DO UPDATE SET
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at,
    cached_until = excluded.cached_until,
    etag         = excluded.etag;
-- name: TouchGlobalSnapshot :execrows
-- ESI answered "not modified": only the bookkeeping moves.
UPDATE global_snapshots
SET fetched_at = $1, cached_until = $2
WHERE kind = $3;
-- name: ListGlobalSnapshots :many
SELECT * FROM global_snapshots
ORDER BY kind;
-- name: GetWarDetail :one
SELECT * FROM war_details
WHERE war_id = $1;
-- name: UpsertWarDetail :exec
INSERT INTO war_details (war_id, payload, fetched_at)
VALUES ($1, $2, $3)
ON CONFLICT (war_id) DO UPDATE SET
    payload    = excluded.payload,
    fetched_at = excluded.fetched_at;
-- name: CountWarDetails :one
SELECT COUNT(*) FROM war_details;

-- ---------------------------------------------------------------------
-- Phase 1B home overview (schema 010): per-account widget layout and
-- the one batched snapshot read every widget renders from.
-- ---------------------------------------------------------------------
-- name: GetUserHomeLayout :one
SELECT home_layout FROM users
WHERE id = $1;
-- name: SetUserHomeLayout :exec
UPDATE users
SET home_layout = $1
WHERE id = $2;

-- Phase 6 (schema 017): the Briefing module's window anchor.
-- name: GetUserBriefingAnchor :one
SELECT last_briefing_at FROM users
WHERE id = $1;
-- name: SetUserBriefingAnchor :exec
UPDATE users
SET last_briefing_at = $1
WHERE id = $2;
-- name: ListSnapshotsForUser :many
SELECT s.character_id, s.kind, s.payload, s.fetched_at, s.cached_until
FROM character_snapshots s
JOIN characters c ON c.character_id = s.character_id
WHERE c.user_id = $1 AND s.kind = ANY(sqlc.arg(kinds)::text[])
ORDER BY s.character_id, s.kind;
-- name: ListPlanetLayoutsForUser :many
SELECT s.character_id, s.kind, s.payload, s.fetched_at
FROM character_snapshots s
JOIN characters c ON c.character_id = s.character_id
WHERE c.user_id = $1 AND strpos(s.kind, 'planet_layout_') = 1
ORDER BY s.character_id, s.kind;

-- ---------------------------------------------------------------------
-- Layout + market toolkit (schema 008): live market suggestions and
-- the item database explorer. All local SDE reads. Matching uses
-- strpos() rather than LIKE: case-insensitive substring positions
-- (1 = a prefix match), no wildcard escaping to worry about.
-- ---------------------------------------------------------------------
-- name: GetWalletHistorySample :one
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = $1 AND character_id = $2 AND day = $3;
-- name: UpsertWalletHistorySample :exec
INSERT INTO wallet_history (user_id, character_id, day, balance, net_worth, sampled_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, character_id, day) DO UPDATE SET
    balance    = excluded.balance,
    net_worth   = excluded.net_worth,
    sampled_at = excluded.sampled_at;
-- name: ListWalletHistorySamples :many
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = $1 AND character_id = $2
ORDER BY day;
-- name: ListUserWalletHistory :many
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = $1
ORDER BY day, character_id;

-- ---------------------------------------------------------------------
-- v0.3.04 widget configuration (schema 020): one JSON blob per
-- (user, widget). The layout (schema 010) owns placement; this
-- owns behaviour (the orders widget's scope + merge mode first).
-- ---------------------------------------------------------------------
-- name: GetWidgetConfig :one
SELECT config FROM widget_configs
WHERE user_id = $1 AND widget_id = $2;
-- name: UpsertWidgetConfig :exec
INSERT INTO widget_configs (user_id, widget_id, config, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, widget_id) DO UPDATE SET
    config     = excluded.config,
    updated_at = excluded.updated_at;
-- name: ListWidgetConfigsByUser :many
SELECT user_id, widget_id, config, updated_at FROM widget_configs
WHERE user_id = $1
ORDER BY widget_id;

-- ---------------------------------------------------------------------
-- v0.3.04 stored market guide (schema 021): the worker mirrors
-- GET /markets/prices/ here wholesale (delete + insert inside
-- one transaction) so asset valuation never waits on a Market
-- page visit. Meta is the single bookkeeping row.
-- ---------------------------------------------------------------------
-- name: CreateSkillPlan :one
INSERT INTO skill_plans (user_id, character_id, name, created_at)
VALUES ($1, $2, $3, $4)
RETURNING id, user_id, character_id, name, created_at;
-- name: ListSkillPlans :many
SELECT id, user_id, character_id, name, created_at FROM skill_plans
WHERE user_id = $1 AND character_id = $2
ORDER BY created_at, id;
-- name: GetSkillPlan :one
SELECT id, user_id, character_id, name, created_at FROM skill_plans
WHERE id = $1 AND user_id = $2;
-- name: DeleteSkillPlan :exec
DELETE FROM skill_plans WHERE id = $1 AND user_id = $2;
-- name: ListSkillPlanItems :many
SELECT plan_id, skill_type_id, target_level, position FROM skill_plan_items
WHERE plan_id = $1
ORDER BY position, skill_type_id;

-- Adding an existing skill raises (or keeps) its target level and
-- leaves position to the caller: a fresh insert takes MAX(position)+1
-- (the handler supplies it).
-- name: UpsertSkillPlanItem :exec
INSERT INTO skill_plan_items (plan_id, skill_type_id, target_level, position)
VALUES ($1, $2, $3, $4)
ON CONFLICT(plan_id, skill_type_id) DO UPDATE SET target_level = excluded.target_level;
-- name: NextSkillPlanPosition :one
SELECT CAST(COALESCE(MAX(position), 0) + 1 AS BIGINT) FROM skill_plan_items WHERE plan_id = $1;
-- name: UpdateSkillPlanItemPosition :exec
UPDATE skill_plan_items SET position = $1 WHERE plan_id = $2 AND skill_type_id = $3;
-- name: DeleteSkillPlanItem :exec
DELETE FROM skill_plan_items WHERE plan_id = $1 AND skill_type_id = $2;

-- ---------------------------------------------------------------------
-- Fitting simulator (schema 030): fits built in the editor, stored
-- per user. items_json is the whole fit document (ship, item lines,
-- charge choices); reads/writes always scope to the owning user.
-- ---------------------------------------------------------------------
-- name: CreateLocalFitting :one
INSERT INTO local_fittings (user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at;
-- name: ListLocalFittings :many
SELECT id, user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at FROM local_fittings
WHERE user_id = $1
ORDER BY updated_at DESC, id DESC
LIMIT 100;
-- name: GetLocalFitting :one
SELECT id, user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at FROM local_fittings
WHERE id = $1 AND user_id = $2;
-- name: UpdateLocalFitting :exec
UPDATE local_fittings
SET name = $1, ship_type_id = $2, items_json = $3, is_public = $4, is_draft = $5, updated_at = $6
WHERE id = $7 AND user_id = $8;
-- name: DeleteLocalFitting :exec
DELETE FROM local_fittings WHERE id = $1 AND user_id = $2;
-- name: GetUserDraftFitting :one
SELECT id, user_id, name, ship_type_id, items_json, is_public, is_draft, created_at, updated_at FROM local_fittings
WHERE user_id = $1 AND is_draft
ORDER BY updated_at DESC, id DESC
LIMIT 1;
-- name: GetPublicFitting :one
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       COALESCE((SELECT c.name FROM characters c WHERE c.user_id = lf.user_id ORDER BY c.character_id LIMIT 1), '') AS author_name
FROM local_fittings lf
WHERE lf.id = $1 AND lf.is_public AND NOT lf.is_draft;
-- name: SearchLocalFittings :many
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.is_public, lf.is_draft, lf.created_at, lf.updated_at,
       COALESCE(tn.name, '') AS ship_name
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
WHERE lf.user_id = sqlc.arg(user_id)
  AND (sqlc.arg(q)::text = '' OR lf.name ILIKE '%' || sqlc.arg(q)::text || '%' OR tn.name ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY lf.updated_at DESC, lf.id DESC
LIMIT 20;
-- name: SearchPublicFittings :many
SELECT lf.id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       COALESCE(tn.name, '') AS ship_name,
       COALESCE((SELECT c.name FROM characters c WHERE c.user_id = lf.user_id ORDER BY c.character_id LIMIT 1), '') AS author_name
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
WHERE lf.is_public AND NOT lf.is_draft AND lf.user_id != sqlc.arg(user_id)
  AND (sqlc.arg(q)::text = '' OR lf.name ILIKE '%' || sqlc.arg(q)::text || '%' OR tn.name ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY lf.updated_at DESC, lf.id DESC
LIMIT 20;

-- ---------------------------------------------------------------------
-- Market history + alerts (schema 013): daily aggregates, wants,
-- fetch state, the watchlist, and worker-computed order health.
-- ---------------------------------------------------------------------

-- ---------------------------------------------------------------------
-- Custom jump-clone names (schema 006): pilot-given labels per clone.
-- ---------------------------------------------------------------------

-- name: UpsertCloneName :exec
INSERT INTO clone_names (user_id, character_id, clone_id, custom_name, updated_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (user_id, character_id, clone_id)
DO UPDATE SET custom_name = EXCLUDED.custom_name, updated_at = now();

-- name: ListCloneNames :many
SELECT character_id, clone_id, custom_name
FROM clone_names
WHERE user_id = $1 AND character_id = $2;

-- name: DeleteCloneName :exec
DELETE FROM clone_names
WHERE user_id = $1 AND character_id = $2 AND clone_id = $3;

-- name: TouchUserSeen :exec
-- The account was seen (worker_tiers.go writes this at most hourly).
UPDATE users SET last_seen_at = $2 WHERE id = $1;

-- name: ListUsersLastSeen :many
SELECT id, last_seen_at FROM users;

-- name: ListUsersBeingNotified :many
-- Accounts whose notifications go somewhere other than the site: a
-- browser subscribed to push, or a linked Discord account (which also
-- has roles to keep right).
SELECT user_id FROM push_subscriptions
UNION
SELECT user_id FROM discord_links;

-- name: FindCharacters :many
-- The Sync and Admin pages' character lookup: names containing the
-- text (its LIKE wildcards already escaped by the caller), or the
-- character with exactly that id. An exact name first, then names
-- that start with the text, then the rest, each alphabetically.
SELECT character_id, name, user_id FROM characters
WHERE name ILIKE '%' || sqlc.arg(pattern)::text || '%'
   OR character_id::text = sqlc.arg(exact)::text
ORDER BY (lower(name) = lower(sqlc.arg(exact)::text)) DESC,
         (name ILIKE sqlc.arg(pattern)::text || '%') DESC,
         lower(name), character_id
LIMIT sqlc.arg(row_limit)::bigint;

-- name: AdminTotals :one
-- The Admin page's headline counts, without reading a row of either
-- table into the page.
SELECT
    (SELECT count(*) FROM users)::bigint AS accounts,
    (SELECT count(*) FROM users WHERE last_seen_at > sqlc.arg(day_ago)::timestamptz)::bigint AS seen_today,
    (SELECT count(*) FROM users WHERE last_seen_at > sqlc.arg(week_ago)::timestamptz)::bigint AS seen_this_week,
    (SELECT count(*) FROM characters)::bigint AS characters,
    (SELECT count(*) FROM characters WHERE link_state NOT IN ('', 'ok'))::bigint AS parked;

-- name: ListNewestUsers :many
-- The most recently created accounts, with how many characters each has.
SELECT u.id, u.created_at, u.last_seen_at,
       (SELECT count(*) FROM characters c WHERE c.user_id = u.id)::bigint AS characters
FROM users u
ORDER BY u.id DESC
LIMIT sqlc.arg(row_limit)::bigint;
