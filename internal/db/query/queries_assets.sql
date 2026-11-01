-- ---------------------------------------------------------------------
-- Assets page location values (schema 118). The worker folds each
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
