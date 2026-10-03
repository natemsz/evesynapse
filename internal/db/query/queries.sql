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

-- name: GetTypeName :one
SELECT name FROM type_names
WHERE type_id = ?;

-- name: UpsertTypeName :exec
INSERT INTO type_names (type_id, name)
VALUES (?, ?)
ON CONFLICT (type_id) DO UPDATE SET
    name = excluded.name;

-- name: SearchTypeNames :many
SELECT type_id, name FROM type_names
WHERE name LIKE ?
ORDER BY name
LIMIT 20;

-- name: ListAllTypeNames :many
SELECT type_id, name FROM type_names
ORDER BY type_id;

-- ---------------------------------------------------------------------
-- SDE static data (schema 003): lookup getters, search, counts, meta.
-- Bulk import inserts are hand-rolled prepared statements inside one
-- transaction in the importer (internal/app/sde.go); reads stay sqlc.
-- ---------------------------------------------------------------------

-- name: GetSDEType :one
SELECT type_id, name, group_id, market_group_id, published FROM sde_types
WHERE type_id = ?;

-- name: GetSDEGroup :one
SELECT group_id, name, category_id FROM sde_groups
WHERE group_id = ?;

-- name: GetSDECategory :one
SELECT category_id, name FROM sde_categories
WHERE category_id = ?;

-- name: GetSDEStation :one
SELECT station_id, name, system_id FROM sde_stations
WHERE station_id = ?;

-- name: GetSDESystem :one
SELECT system_id, name, region_id, security FROM sde_systems
WHERE system_id = ?;

-- name: GetSDERegion :one
SELECT region_id, name FROM sde_regions
WHERE region_id = ?;

-- name: SearchSDETypes :many
SELECT type_id, name FROM sde_types
WHERE name LIKE ?
ORDER BY name
LIMIT 20;

-- name: ListSDETypeIDs :many
SELECT type_id FROM sde_types
ORDER BY type_id;

-- name: CountSDETypes :one
SELECT COUNT(*) FROM sde_types;

-- name: CountSDEGroups :one
SELECT COUNT(*) FROM sde_groups;

-- name: CountSDECategories :one
SELECT COUNT(*) FROM sde_categories;

-- name: CountSDEStations :one
SELECT COUNT(*) FROM sde_stations;

-- name: CountSDESystems :one
SELECT COUNT(*) FROM sde_systems;

-- name: CountSDERegions :one
SELECT COUNT(*) FROM sde_regions;

-- name: GetSDEMeta :one
SELECT value FROM sde_meta
WHERE key = ?;

-- name: UpsertSDEMeta :exec
INSERT INTO sde_meta (key, value)
VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET
    value = excluded.value;

-- ---------------------------------------------------------------------
-- Module sweep (schema 004): killmail detail store. The worker warms
-- details from the recent-killmails snapshot; pages only read here.
-- ---------------------------------------------------------------------

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

-- name: GetItemName :one
SELECT name FROM item_names
WHERE item_id = ?;

-- name: ListItemNames :many
SELECT item_id, name FROM item_names
ORDER BY item_id;

-- name: UpsertItemName :exec
INSERT INTO item_names (item_id, name)
VALUES (?, ?)
ON CONFLICT (item_id) DO UPDATE SET
    name = excluded.name;

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

-- name: SuggestSDETypes :many
SELECT type_id, name FROM sde_types
WHERE market_group_id > 0 AND published = 1
  AND instr(lower(name), lower(?1)) > 0
ORDER BY CASE WHEN instr(lower(name), lower(?1)) = 1 THEN 0 ELSE 1 END, name
LIMIT 10;

-- name: ListSDECategoriesWithCounts :many
SELECT c.category_id, c.name, COUNT(t.type_id) AS type_count
FROM sde_categories c
LEFT JOIN sde_groups g ON g.category_id = c.category_id
LEFT JOIN sde_types t ON t.group_id = g.group_id
GROUP BY c.category_id, c.name
ORDER BY c.name;

-- name: ListSDEGroupsInCategory :many
SELECT g.group_id, g.name, COUNT(t.type_id) AS type_count
FROM sde_groups g
LEFT JOIN sde_types t ON t.group_id = g.group_id
WHERE g.category_id = ?
GROUP BY g.group_id, g.name
ORDER BY g.name;

-- name: CountSDETypesInGroupFiltered :one
SELECT COUNT(*) FROM sde_types
WHERE group_id = ? AND instr(lower(name), lower(?)) > 0;

-- name: ListSDETypesInGroup :many
SELECT type_id, name, market_group_id FROM sde_types
WHERE group_id = ? AND instr(lower(name), lower(?)) > 0
ORDER BY name
LIMIT ? OFFSET ?;

-- ---------------------------------------------------------------------
-- Phase 3 (schema 011): industry build planner reads. Bulk import
-- inserts stay hand-rolled in the SDE importer alongside the other
-- sde_* tables; only reads live here.
-- ---------------------------------------------------------------------

-- name: CountSDEBlueprints :one
SELECT COUNT(*) FROM sde_blueprints;

-- name: GetSDEBlueprintForProduct :one
SELECT blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds
FROM sde_blueprints
WHERE product_type_id = ?
ORDER BY blueprint_type_id
LIMIT 1;

-- name: GetSDEBlueprint :one
SELECT blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds
FROM sde_blueprints
WHERE blueprint_type_id = ?;

-- name: ListSDEBlueprintMaterials :many
SELECT material_type_id, quantity FROM sde_blueprint_materials
WHERE blueprint_type_id = ?
ORDER BY material_type_id;

-- name: ListSDEBlueprintSkills :many
SELECT skill_type_id, level FROM sde_blueprint_skills
WHERE blueprint_type_id = ?
ORDER BY level DESC, skill_type_id;

-- name: SearchManufacturableProducts :many
SELECT t.type_id, t.name, b.blueprint_type_id
FROM sde_blueprints b
JOIN sde_types t ON t.type_id = b.product_type_id
WHERE t.published = 1 AND instr(lower(t.name), lower(?1)) > 0
ORDER BY CASE WHEN instr(lower(t.name), lower(?1)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT 50;
