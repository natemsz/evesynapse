-- ---------------------------------------------------------------------
-- Assets page location values (schema 119). The worker folds each
-- character's stored asset snapshot into one row per place, priced
-- at the house order; the Assets page directory reads the rows.
-- ---------------------------------------------------------------------

-- name: UpsertAssetLocationValue :exec
INSERT INTO asset_location_values (
    user_id, character_id, location_id, location_type,
    est_value, priced_stacks, total_stacks, total_items, ships, volume, computed_at
) VALUES (
    sqlc.arg(user_id), sqlc.arg(character_id), sqlc.arg(location_id), sqlc.arg(location_type),
    sqlc.arg(est_value), sqlc.arg(priced_stacks), sqlc.arg(total_stacks), sqlc.arg(total_items),
    sqlc.arg(ships), sqlc.arg(volume), sqlc.arg(computed_at)
)
ON CONFLICT (user_id, character_id, location_id) DO UPDATE SET
    location_type = EXCLUDED.location_type,
    est_value = EXCLUDED.est_value,
    priced_stacks = EXCLUDED.priced_stacks,
    total_stacks = EXCLUDED.total_stacks,
    total_items = EXCLUDED.total_items,
    ships = EXCLUDED.ships,
    volume = EXCLUDED.volume,
    computed_at = EXCLUDED.computed_at;

-- name: DeleteAssetLocationValuesForCharacter :exec
DELETE FROM asset_location_values
WHERE user_id = sqlc.arg(user_id) AND character_id = sqlc.arg(character_id);

-- name: ListAssetLocationValuesByUser :many
SELECT user_id, character_id, location_id, location_type,
       est_value, priced_stacks, total_stacks, total_items, ships, volume, computed_at
FROM asset_location_values
WHERE user_id = sqlc.arg(user_id)
ORDER BY location_id, character_id;

-- The newest fold for one character, for the freshness check
-- against the snapshot it was folded from.
-- name: GetAssetLocationValueLatest :one
SELECT computed_at FROM asset_location_values
WHERE user_id = sqlc.arg(user_id) AND character_id = sqlc.arg(character_id)
ORDER BY computed_at DESC
LIMIT 1;

-- The SDE meta group (dogma attribute 1692) of each of some
-- types, for the matrix rows' tech pips: 2 Tech II, 14 Tech III,
-- 4 faction, 6 deadspace, 5 officer, 3 storyline, 1 Tech I.
-- name: ListAssetTypeMetaGroups :many
SELECT type_id, value FROM sde_type_attributes
WHERE attribute_id = 1692
  AND type_id = ANY(sqlc.arg(type_ids)::bigint[]);

-- Places a character no longer holds anything in lose their row
-- on the next fold; the fold upserts the current set first, then
-- this sweep removes what it did not rewrite.
-- name: DeleteAssetLocationValuesExcept :exec
DELETE FROM asset_location_values
WHERE user_id = sqlc.arg(user_id) AND character_id = sqlc.arg(character_id)
  AND NOT (location_id = ANY(sqlc.arg(keep_location_ids)::bigint[]));

-- ---------------------------------------------------------------------
-- Blueprint buildability (schema 122): per owned blueprint, folded by
-- the worker from an account's blueprint, asset and skill snapshots.

-- name: GetBlueprintBuildableFold :one
SELECT characters, computed_at FROM blueprint_buildable_folds
WHERE user_id = $1;

-- name: UpsertBlueprintBuildableFold :exec
INSERT INTO blueprint_buildable_folds (user_id, characters, computed_at)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET
    characters  = excluded.characters,
    computed_at = excluded.computed_at;

-- name: UpsertBlueprintBuildable :exec
INSERT INTO blueprint_buildable (
    user_id, character_id, item_id, blueprint_type, total_materials,
    has_skills, has_materials, missing_skills, short_materials,
    any_skills, any_materials, any_missing_skills, any_short_materials,
    computed_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
ON CONFLICT (character_id, item_id) DO UPDATE SET
    user_id             = excluded.user_id,
    blueprint_type      = excluded.blueprint_type,
    total_materials     = excluded.total_materials,
    has_skills          = excluded.has_skills,
    has_materials       = excluded.has_materials,
    missing_skills      = excluded.missing_skills,
    short_materials     = excluded.short_materials,
    any_skills          = excluded.any_skills,
    any_materials       = excluded.any_materials,
    any_missing_skills  = excluded.any_missing_skills,
    any_short_materials = excluded.any_short_materials,
    computed_at         = excluded.computed_at;

-- name: DeleteBlueprintBuildableOlder :exec
-- Drops the account's verdicts the latest fold did not write: the
-- blueprints its characters no longer own.
DELETE FROM blueprint_buildable
WHERE user_id = sqlc.arg(user_id) AND computed_at < sqlc.arg(fold_at);

-- name: ListBlueprintBuildable :many
-- The stored verdicts for the blueprints a page shows.
SELECT item_id, total_materials,
       has_skills, has_materials, missing_skills, short_materials,
       any_skills, any_materials, any_missing_skills, any_short_materials
FROM blueprint_buildable
WHERE user_id = sqlc.arg(user_id) AND item_id = ANY(sqlc.arg(item_ids)::bigint[]);

-- name: ListSnapshotStamps :many
-- When each of some characters' snapshots of some kinds was last
-- fetched, without their payloads: a fold's watermark check.
SELECT character_id, kind, fetched_at FROM character_snapshots
WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND kind = ANY(sqlc.arg(kinds)::text[]);
