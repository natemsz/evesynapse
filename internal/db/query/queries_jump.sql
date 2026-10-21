-- ---------------------------------------------------------------------
-- The map's jump planner (map_jump.go): where systems truly are, and
-- which ships jump how far, both from the static data export.
-- ---------------------------------------------------------------------

-- Every known-space system's position (metres) and region.
-- name: ListJumpCoords :many
SELECT system_id, region_id, x, y, z FROM sde_systems
WHERE system_id >= 30000000 AND system_id < 31000000
ORDER BY system_id;

-- Every published ship with a jump drive: its base range (light
-- years, attribute jumpDriveRange) and, where the type carries one,
-- its jump fatigue multiplier.
-- name: ListJumpShips :many
SELECT t.type_id, t.name, g.name AS group_name, r.value AS jump_range,
       COALESCE((SELECT f.value FROM sde_type_attributes f
                 JOIN sde_attribute_types fa ON fa.attribute_id = f.attribute_id
                 WHERE f.type_id = t.type_id AND fa.name = 'jumpFatigueMultiplier' LIMIT 1), 0)::float8 AS fatigue_multiplier
FROM sde_type_attributes r
JOIN sde_attribute_types ra ON ra.attribute_id = r.attribute_id AND ra.name = 'jumpDriveRange'
JOIN sde_types t ON t.type_id = r.type_id
JOIN sde_groups g ON g.group_id = t.group_id
WHERE r.value > 0 AND t.published = 1 AND g.category_id = 6
ORDER BY g.name, t.name;
