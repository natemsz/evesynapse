-- name: CreateUser :one
INSERT INTO users (created_at)
VALUES (datetime('now'))
RETURNING *;
-- name: GetUser :one
SELECT * FROM users
WHERE id = ?;
-- name: UpsertCharacter :one
INSERT INTO characters (
    character_id, user_id, name,
    access_token, refresh_token, token_expiry,
    scopes, cached_until, owner_hash, link_state, link_state_at,
    updated_at
) VALUES (
    ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?, ?, ?,
    datetime('now')
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
WHERE character_id = ?;
-- name: ListCharactersByUser :many
SELECT * FROM characters
WHERE user_id = ?
ORDER BY name;
-- name: DeleteCharacter :exec
DELETE FROM characters
WHERE character_id = ? AND user_id = ?;
-- name: SetCharacterTags :exec
UPDATE characters
SET tags = ?, updated_at = datetime('now')
WHERE character_id = ? AND user_id = ?;
-- name: SetCharacterLinkState :exec
UPDATE characters
SET link_state = ?, link_state_at = ?, updated_at = datetime('now')
WHERE character_id = ?;
-- name: ListUsers :many
SELECT * FROM users
ORDER BY id;
-- name: ListAllCharacters :many
SELECT * FROM characters
ORDER BY user_id, name;
-- name: UpdateCharacterTokens :exec
UPDATE characters
SET access_token = ?, refresh_token = ?, token_expiry = ?, updated_at = datetime('now')
WHERE character_id = ?;
-- name: GetSnapshot :one
SELECT * FROM character_snapshots
WHERE character_id = ? AND kind = ?;
-- name: UpsertSnapshot :exec
INSERT INTO character_snapshots (character_id, kind, payload, fetched_at, cached_until)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (character_id, kind) DO UPDATE SET
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at,
    cached_until = excluded.cached_until;
-- name: ListSnapshotsByCharacter :many
SELECT * FROM character_snapshots
WHERE character_id = ?
ORDER BY kind;
-- name: ListSnapshotsByKind :many
SELECT * FROM character_snapshots
WHERE kind = ?
ORDER BY character_id;
-- name: GetKillmailDetail :one
SELECT * FROM killmail_details
WHERE killmail_id = ?;
-- name: ListKillmailDetailIDsByCharacter :many
SELECT killmail_id FROM killmail_details
WHERE character_id = ?
ORDER BY killmail_id;
-- name: ListKillmailDetailsByCharacter :many
SELECT * FROM killmail_details
WHERE character_id = ?
ORDER BY killmail_id;
-- name: ListRecentKillmailDetails :many
SELECT payload FROM killmail_details
ORDER BY fetched_at DESC
LIMIT ?;
-- name: ListLiquidCoreTypes :many
SELECT mh.type_id, SUM(mh.volume * mh.average) AS isk_velocity
FROM market_history mh
WHERE mh.region_id = sqlc.arg(region_id)
  AND mh.date >= (SELECT date(MAX(mh2.date), '-7 days') FROM market_history mh2 WHERE mh2.region_id = sqlc.arg(region_id))
GROUP BY mh.type_id
ORDER BY isk_velocity DESC
LIMIT sqlc.arg(core_limit);
-- name: UpsertKillmailDetail :exec
INSERT INTO killmail_details (killmail_id, character_id, hash, payload, fetched_at)
VALUES (?, ?, ?, ?, ?)
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
WHERE contract_id = ?;
-- name: ListContractDetailIDsByCharacter :many
SELECT contract_id FROM contract_details
WHERE character_id = ?
ORDER BY contract_id;
-- name: UpsertContractDetail :exec
INSERT INTO contract_details (contract_id, character_id, payload, fetched_at)
VALUES (?, ?, ?, ?)
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
WHERE character_id = ? AND kind = ?;
-- name: ListSnapshotFetchStatesByCharacter :many
SELECT * FROM snapshot_fetch_state
WHERE character_id = ?
ORDER BY kind;
-- name: UpsertSnapshotFetchState :exec
INSERT INTO snapshot_fetch_state (character_id, kind, state, detail, attempted_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (character_id, kind) DO UPDATE SET
    state        = excluded.state,
    detail       = excluded.detail,
    attempted_at = excluded.attempted_at;
-- name: GetCharacterCorporation :one
SELECT * FROM character_corporations
WHERE character_id = ?;
-- name: UpsertCharacterCorporation :exec
INSERT INTO character_corporations (character_id, corporation_id, updated_at)
VALUES (?, ?, ?)
ON CONFLICT (character_id) DO UPDATE SET
    corporation_id = excluded.corporation_id,
    updated_at     = excluded.updated_at;
-- name: ListAllCorporationIDs :many
SELECT DISTINCT corporation_id FROM character_corporations
ORDER BY corporation_id;

-- ---------------------------------------------------------------------
-- Module sweep, cluster 4 (schema 007): intel public-data store.
-- Global snapshots are the public-data counterpart of
-- character_snapshots; war details mirror killmail_details.
-- ---------------------------------------------------------------------
-- name: GetGlobalSnapshot :one
SELECT * FROM global_snapshots
WHERE kind = ?;
-- name: UpsertGlobalSnapshot :exec
INSERT INTO global_snapshots (kind, payload, fetched_at, cached_until)
VALUES (?, ?, ?, ?)
ON CONFLICT (kind) DO UPDATE SET
    payload      = excluded.payload,
    fetched_at   = excluded.fetched_at,
    cached_until = excluded.cached_until;
-- name: ListGlobalSnapshots :many
SELECT * FROM global_snapshots
ORDER BY kind;
-- name: GetWarDetail :one
SELECT * FROM war_details
WHERE war_id = ?;
-- name: UpsertWarDetail :exec
INSERT INTO war_details (war_id, payload, fetched_at)
VALUES (?, ?, ?)
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
WHERE id = ?;
-- name: SetUserHomeLayout :exec
UPDATE users
SET home_layout = ?
WHERE id = ?;

-- Phase 6 (schema 017): the Briefing module's window anchor.
-- name: GetUserBriefingAnchor :one
SELECT last_briefing_at FROM users
WHERE id = ?;
-- name: SetUserBriefingAnchor :exec
UPDATE users
SET last_briefing_at = ?
WHERE id = ?;
-- name: ListSnapshotsForUser :many
SELECT s.character_id, s.kind, s.payload, s.fetched_at, s.cached_until
FROM character_snapshots s
JOIN characters c ON c.character_id = s.character_id
WHERE c.user_id = ? AND s.kind IN (sqlc.slice('kinds'))
ORDER BY s.character_id, s.kind;
-- name: ListPlanetLayoutsForUser :many
SELECT s.character_id, s.kind, s.payload, s.fetched_at
FROM character_snapshots s
JOIN characters c ON c.character_id = s.character_id
WHERE c.user_id = ? AND instr(s.kind, 'planet_layout_') = 1
ORDER BY s.character_id, s.kind;

-- ---------------------------------------------------------------------
-- Layout + market toolkit (schema 008): live market suggestions and
-- the item database explorer. All local SDE reads. Matching uses
-- instr() rather than LIKE: case-insensitive substring positions
-- (1 = a prefix match), no wildcard escaping to worry about.
-- ---------------------------------------------------------------------
-- name: GetWalletHistorySample :one
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = ? AND character_id = ? AND day = ?;
-- name: UpsertWalletHistorySample :exec
INSERT INTO wallet_history (user_id, character_id, day, balance, net_worth, sampled_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (user_id, character_id, day) DO UPDATE SET
    balance    = excluded.balance,
    net_worth   = excluded.net_worth,
    sampled_at = excluded.sampled_at;
-- name: ListWalletHistorySamples :many
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = ? AND character_id = ?
ORDER BY day;
-- name: ListUserWalletHistory :many
SELECT user_id, character_id, day, balance, net_worth, sampled_at
FROM wallet_history
WHERE user_id = ?
ORDER BY day, character_id;

-- ---------------------------------------------------------------------
-- v0.3.04 widget configuration (schema 020): one JSON blob per
-- (user, widget). The layout (schema 010) owns placement; this
-- owns behaviour (the orders widget's scope + merge mode first).
-- ---------------------------------------------------------------------
-- name: GetWidgetConfig :one
SELECT config FROM widget_configs
WHERE user_id = ? AND widget_id = ?;
-- name: UpsertWidgetConfig :exec
INSERT INTO widget_configs (user_id, widget_id, config, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (user_id, widget_id) DO UPDATE SET
    config     = excluded.config,
    updated_at = excluded.updated_at;
-- name: ListWidgetConfigsByUser :many
SELECT user_id, widget_id, config, updated_at FROM widget_configs
WHERE user_id = ?
ORDER BY widget_id;

-- ---------------------------------------------------------------------
-- v0.3.04 stored market guide (schema 021): the worker mirrors
-- GET /markets/prices/ here wholesale (delete + insert inside
-- one transaction) so asset valuation never waits on a Market
-- page visit. Meta is the single bookkeeping row.
-- ---------------------------------------------------------------------
-- name: CreateSkillPlan :one
INSERT INTO skill_plans (user_id, character_id, name, created_at)
VALUES (?, ?, ?, ?)
RETURNING id, user_id, character_id, name, created_at;
-- name: ListSkillPlans :many
SELECT id, user_id, character_id, name, created_at FROM skill_plans
WHERE user_id = ? AND character_id = ?
ORDER BY created_at, id;
-- name: GetSkillPlan :one
SELECT id, user_id, character_id, name, created_at FROM skill_plans
WHERE id = ? AND user_id = ?;
-- name: DeleteSkillPlan :exec
DELETE FROM skill_plans WHERE id = ? AND user_id = ?;
-- name: ListSkillPlanItems :many
SELECT plan_id, skill_type_id, target_level, position FROM skill_plan_items
WHERE plan_id = ?
ORDER BY position, skill_type_id;

-- Adding an existing skill raises (or keeps) its target level and
-- leaves position to the caller: a fresh insert takes MAX(position)+1
-- (the handler supplies it).
-- name: UpsertSkillPlanItem :exec
INSERT INTO skill_plan_items (plan_id, skill_type_id, target_level, position)
VALUES (?, ?, ?, ?)
ON CONFLICT(plan_id, skill_type_id) DO UPDATE SET target_level = excluded.target_level;
-- name: NextSkillPlanPosition :one
SELECT COALESCE(MAX(position), 0) + 1 FROM skill_plan_items WHERE plan_id = ?;
-- name: UpdateSkillPlanItemPosition :exec
UPDATE skill_plan_items SET position = ? WHERE plan_id = ? AND skill_type_id = ?;
-- name: DeleteSkillPlanItem :exec
DELETE FROM skill_plan_items WHERE plan_id = ? AND skill_type_id = ?;

-- ---------------------------------------------------------------------
-- Fitting simulator (schema 030): fits built in the editor, stored
-- per user. items_json is the whole fit document (ship, item lines,
-- charge choices); reads/writes always scope to the owning user.
-- ---------------------------------------------------------------------
-- name: CreateLocalFitting :one
INSERT INTO local_fittings (user_id, name, ship_type_id, items_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING id, user_id, name, ship_type_id, items_json, created_at, updated_at;
-- name: ListLocalFittings :many
SELECT id, user_id, name, ship_type_id, items_json, created_at, updated_at FROM local_fittings
WHERE user_id = ?
ORDER BY updated_at DESC, id DESC
LIMIT 100;
-- name: GetLocalFitting :one
SELECT id, user_id, name, ship_type_id, items_json, created_at, updated_at FROM local_fittings
WHERE id = ? AND user_id = ?;
-- name: UpdateLocalFitting :exec
UPDATE local_fittings
SET name = ?, ship_type_id = ?, items_json = ?, updated_at = ?
WHERE id = ? AND user_id = ?;
-- name: DeleteLocalFitting :exec
DELETE FROM local_fittings WHERE id = ? AND user_id = ?;

-- ---------------------------------------------------------------------
-- Market history + alerts (schema 013): daily aggregates, wants,
-- fetch state, the watchlist, and worker-computed order health.
-- ---------------------------------------------------------------------
