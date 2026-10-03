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
    scopes, cached_until, updated_at
) VALUES (
    ?, ?, ?,
    ?, ?, ?,
    ?, ?, datetime('now')
)
ON CONFLICT (character_id) DO UPDATE SET
    user_id       = excluded.user_id,
    name          = excluded.name,
    access_token  = excluded.access_token,
    refresh_token = excluded.refresh_token,
    token_expiry  = excluded.token_expiry,
    scopes        = excluded.scopes,
    cached_until  = excluded.cached_until,
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
SELECT type_id, name, group_id FROM sde_types
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
