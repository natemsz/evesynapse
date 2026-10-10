-- name: GetTypeName :one
SELECT name FROM type_names
WHERE type_id = $1;
-- name: UpsertTypeName :exec
INSERT INTO type_names (type_id, name)
VALUES ($1, $2)
ON CONFLICT (type_id) DO UPDATE SET
    name = excluded.name;
-- name: SearchTypeNames :many
SELECT type_id, name FROM type_names
WHERE name ILIKE $1
ORDER BY name
LIMIT 20;
-- name: ListAllTypeNames :many
SELECT type_id, name FROM type_names
ORDER BY type_id;
-- name: ListTypeNameIDsByIDs :many
-- Batched name-cache hit check: which of the given type IDs
-- have a resolved name, without pulling the whole table.
SELECT type_id FROM type_names
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- name: ListSDETypeIDsByIDs :many
-- Batched SDE hit check: which of the given type IDs the SDE
-- knows, without pulling all 53K type IDs.
SELECT type_id FROM sde_types
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);

-- ---------------------------------------------------------------------
-- SDE static data (schema 003): lookup getters, search, counts, meta.
-- Bulk import inserts are hand-rolled prepared statements inside one
-- transaction in the importer (internal/app/sde.go); reads stay sqlc.
-- ---------------------------------------------------------------------
-- name: GetSDEType :one
SELECT type_id, name, group_id, market_group_id, published, description FROM sde_types
WHERE type_id = $1;
-- The group each of some types is in, by name: a ship's class.
-- name: ListTypeGroupNames :many
SELECT t.type_id, g.name FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- name: GetSDEGroup :one
SELECT group_id, name, category_id FROM sde_groups
WHERE group_id = $1;
-- name: GetSDECategory :one
SELECT category_id, name FROM sde_categories
WHERE category_id = $1;
-- name: GetSDEStation :one
SELECT station_id, name, system_id FROM sde_stations
WHERE station_id = $1;
-- name: ListSDEStationsBySystem :many
SELECT station_id, name, system_id FROM sde_stations
WHERE system_id = $1
ORDER BY name;
-- name: GetSDESystem :one
SELECT system_id, name, region_id, security FROM sde_systems
WHERE system_id = $1;
-- name: GetSDERegion :one
SELECT region_id, name FROM sde_regions
WHERE region_id = $1;
-- name: SearchSDETypes :many
SELECT type_id, name FROM sde_types
WHERE name ILIKE $1
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

-- Market browse tree (schema 024): the invMarketGroups hierarchy.
-- Reads only; the bulk import stays hand-rolled in sde.go like the
-- other SDE tables. Listed types keep the app-wide marketable floor
-- (published = 1 AND market_group_id > 0).
-- name: GetSDEMarketGroup :one
SELECT market_group_id, parent_group_id, name, icon_id, has_types FROM sde_market_groups
WHERE market_group_id = $1;
-- name: ListSDEMarketGroupsByParent :many
SELECT market_group_id, parent_group_id, name, icon_id, has_types FROM sde_market_groups
WHERE parent_group_id = $1
ORDER BY name;
-- name: ListSDETypesInMarketGroup :many
SELECT type_id, name FROM sde_types
WHERE market_group_id = $1 AND published = 1 AND market_group_id > 0
ORDER BY name;
-- name: ListSDETypesInMarketGroupPaged :many
-- One page of a market group's types: the market browse tab
-- pages big groups instead of pulling thousands of rows.
SELECT type_id, name FROM sde_types
WHERE market_group_id = $1 AND published = 1 AND market_group_id > 0
ORDER BY name
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;
-- name: CountSDETypesInMarketGroup :one
SELECT COUNT(*) FROM sde_types
WHERE market_group_id = $1 AND published = 1 AND market_group_id > 0;
-- name: CountSDEMarketGroups :one
SELECT COUNT(*) FROM sde_market_groups;
-- name: GetSDEMeta :one
SELECT value FROM sde_meta
WHERE key = $1;
-- name: UpsertSDEMeta :exec
INSERT INTO sde_meta (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET
    value = excluded.value;

-- ---------------------------------------------------------------------
-- Killmail detail store. The worker warms
-- details from the recent-killmails snapshot; pages only read here.
-- ---------------------------------------------------------------------
-- name: GetItemName :one
SELECT name FROM item_names
WHERE item_id = $1;
-- name: ListItemNames :many
SELECT item_id, name FROM item_names
ORDER BY item_id;
-- name: ListItemNamesByIDs :many
-- Batched singleton-name lookup: the names for one page's item
-- IDs, without pulling the whole item_names table.
SELECT item_id, name FROM item_names
WHERE item_id = ANY(sqlc.arg(item_ids)::bigint[]);
-- name: UpsertItemName :exec
INSERT INTO item_names (item_id, name)
VALUES ($1, $2)
ON CONFLICT (item_id) DO UPDATE SET
    name = excluded.name;
-- name: SuggestSDETypes :many
SELECT type_id, name FROM sde_types
WHERE market_group_id > 0 AND published = 1
  AND strpos(lower(name), lower($1)) > 0
ORDER BY CASE WHEN strpos(lower(name), lower($1)) = 1 THEN 0 ELSE 1 END, name
LIMIT 10;
-- name: ListSDECategoriesWithCounts :many
SELECT c.category_id, c.name, COUNT(t.type_id) AS type_count
FROM sde_categories c
LEFT JOIN sde_groups g ON g.category_id = c.category_id
LEFT JOIN sde_types t ON t.group_id = g.group_id
GROUP BY c.category_id, c.name
ORDER BY c.name;
-- name: ListSDECategoriesWithCountsFiltered :many
SELECT c.category_id, c.name, COUNT(t.type_id) AS type_count
FROM sde_categories c
LEFT JOIN sde_groups g ON g.category_id = c.category_id
LEFT JOIN sde_types t ON t.group_id = g.group_id
  AND (CAST(@market_only AS BIGINT) = 0 OR (t.market_group_id > 0 AND t.published = 1))
WHERE CAST(@market_only AS BIGINT) = 0 OR EXISTS (
  SELECT 1 FROM sde_groups g2
  JOIN sde_types t2 ON t2.group_id = g2.group_id
  WHERE g2.category_id = c.category_id AND t2.market_group_id > 0 AND t2.published = 1
)
GROUP BY c.category_id, c.name
ORDER BY c.name;
-- name: ListSDEGroupsInCategory :many
SELECT g.group_id, g.name, COUNT(t.type_id) AS type_count
FROM sde_groups g
LEFT JOIN sde_types t ON t.group_id = g.group_id
WHERE g.category_id = $1
GROUP BY g.group_id, g.name
ORDER BY g.name;
-- name: ListSDEGroupsInCategoryFiltered :many
SELECT g.group_id, g.name, COUNT(t.type_id) AS type_count
FROM sde_groups g
LEFT JOIN sde_types t ON t.group_id = g.group_id
  AND (CAST(@market_only AS BIGINT) = 0 OR (t.market_group_id > 0 AND t.published = 1))
WHERE g.category_id = sqlc.arg(category_id)
  AND (CAST(@market_only AS BIGINT) = 0 OR EXISTS (
    SELECT 1 FROM sde_types t2
    WHERE t2.group_id = g.group_id AND t2.market_group_id > 0 AND t2.published = 1
  ))
GROUP BY g.group_id, g.name
ORDER BY g.name;
-- name: CountSDETypesInGroupFiltered :one
SELECT COUNT(*) FROM sde_types
WHERE group_id = $1 AND strpos(lower(name), lower($2)) > 0
  AND (CAST(@market_only AS BIGINT) = 0 OR (market_group_id > 0 AND published = 1));
-- name: ListSDETypesInGroup :many
SELECT type_id, name, market_group_id FROM sde_types
WHERE group_id = $1 AND strpos(lower(name), lower($2)) > 0
  AND (CAST(@market_only AS BIGINT) = 0 OR (market_group_id > 0 AND published = 1))
ORDER BY name
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;

-- ---------------------------------------------------------------------
-- Next-1 (search & findability): the Items DB global search and
-- the one shared suggestion feed behind every autocomplete box.
-- All local SDE reads; the market-only predicate is the same
-- marketable+published pair everywhere it appears.
-- ---------------------------------------------------------------------
-- name: SearchSDETypesFiltered :many
SELECT t.type_id, t.name, t.group_id, t.market_group_id, t.published,
       COALESCE(g.name, '') AS group_name,
       COALESCE(g.category_id, 0) AS category_id,
       COALESCE(c.name, '') AS category_name
FROM sde_types t
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_categories c ON c.category_id = g.category_id
WHERE strpos(lower(t.name), lower(@q)) > 0
  AND (CAST(@market_only AS BIGINT) = 0 OR (t.market_group_id > 0 AND t.published = 1))
  AND (CAST(@category_id AS BIGINT) = 0 OR g.category_id = @category_id)
  AND (CAST(@group_id AS BIGINT) = 0 OR t.group_id = @group_id)
-- Category and group first, so a page of results reads as the browse
-- tree does (consecutive rows share a heading); within a group, names
-- that start with the query come before names that merely contain it.
-- (The relevance term names @q: a raw $1 here became a second, never-set
-- parameter, so every name counted as a prefix match and the term did nothing.)
ORDER BY COALESCE(c.name, ''), COALESCE(g.name, ''),
         CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint OFFSET @off::bigint;
-- name: CountSDETypesFiltered :one
SELECT COUNT(*)
FROM sde_types t
LEFT JOIN sde_groups g ON g.group_id = t.group_id
WHERE strpos(lower(t.name), lower(@q)) > 0
  AND (CAST(@market_only AS BIGINT) = 0 OR (t.market_group_id > 0 AND t.published = 1))
  AND (CAST(@category_id AS BIGINT) = 0 OR g.category_id = @category_id)
  AND (CAST(@group_id AS BIGINT) = 0 OR t.group_id = @group_id);
-- name: SuggestSDETypesShared :many
SELECT t.type_id, t.name,
       COALESCE(g.name, '') AS group_name,
       COALESCE(c.name, '') AS category_name
FROM sde_types t
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_categories c ON c.category_id = g.category_id
WHERE strpos(lower(t.name), lower(@q)) > 0
  AND t.published = 1 AND t.market_group_id > 0
  AND (CAST(@pool AS TEXT) != 'planner' OR EXISTS (
        SELECT 1 FROM sde_blueprints b WHERE b.product_type_id = t.type_id))
  AND (CAST(@pool AS TEXT) != 'skills' OR EXISTS (
        SELECT 1 FROM sde_skill_meta m WHERE m.type_id = t.type_id))
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;

-- Pilot-name search for the top banner: ready records whose
-- stored payload mentions the text; the handler re-checks the
-- pilot's own name field before offering a row, so bios that
-- merely mention a name never produce a hit.
-- name: SearchPilotRecordsByName :many
SELECT character_id, payload
FROM pilot_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(payload), lower($1)) > 0
ORDER BY character_id
LIMIT 100;

-- Corporation-name search for the top banner and quick jump:
-- ready records whose stored payload mentions the text; the
-- handler re-checks the corporation's own name field before
-- offering a row, same as the pilot search above.
-- name: SearchCorporationRecordsByName :many
SELECT corporation_id, payload
FROM corporation_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(payload), lower($1)) > 0
ORDER BY corporation_id
LIMIT 100;

-- Alliance-name search, same posture as the corporation one.
-- name: SearchAllianceRecordsByName :many
SELECT alliance_id, payload
FROM alliance_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(payload), lower($1)) > 0
ORDER BY alliance_id
LIMIT 100;

-- ---------------------------------------------------------------------
-- The daily wallet-history sampler.
-- One row per character per day; the upsert keeps the day's
-- latest values as fresher snapshots land.
-- ---------------------------------------------------------------------
-- name: CountSDEBlueprints :one
SELECT COUNT(*) FROM sde_blueprints;
-- name: ListSDEBlueprintProducts :many
SELECT blueprint_type_id, product_type_id FROM sde_blueprints;
-- name: GetSDEBlueprintForProduct :one
SELECT blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds
FROM sde_blueprints
WHERE product_type_id = $1
ORDER BY blueprint_type_id
LIMIT 1;
-- name: GetSDEBlueprint :one
SELECT blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds
FROM sde_blueprints
WHERE blueprint_type_id = $1;
-- name: ListSDEBlueprintMaterials :many
SELECT material_type_id, quantity FROM sde_blueprint_materials
WHERE blueprint_type_id = $1
ORDER BY material_type_id;
-- name: ListSDEBlueprintSkills :many
SELECT skill_type_id, level FROM sde_blueprint_skills
WHERE blueprint_type_id = $1
ORDER BY level DESC, skill_type_id;
-- name: SearchManufacturableProducts :many
SELECT t.type_id, t.name, b.blueprint_type_id
FROM sde_blueprints b
JOIN sde_types t ON t.type_id = b.product_type_id
WHERE t.published = 1 AND strpos(lower(t.name), lower($1)) > 0
ORDER BY CASE WHEN strpos(lower(t.name), lower($1)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT 50;

-- ---------------------------------------------------------------------
-- Skill graph reads and user skill plans.
-- The dogma bulk inserts stay hand-rolled in the SDE importer
-- alongside the other sde_* tables; only reads live here. Plans
-- are plain CRUD.
-- ---------------------------------------------------------------------
-- name: CountSDESkillMeta :one
SELECT COUNT(*) FROM sde_skill_meta;
-- name: CountSDERequirements :one
SELECT COUNT(*) FROM sde_requirements;

-- The browsable skill catalog: every published skill with its
-- group, rank and training attributes.
-- name: ListSDESkillCatalog :many
SELECT m.type_id, t.name, g.name AS group_name, m.rank, m.primary_attr, m.secondary_attr
FROM sde_skill_meta m
JOIN sde_types t ON t.type_id = m.type_id
JOIN sde_groups g ON g.group_id = t.group_id
ORDER BY g.name, t.name;
-- name: GetSDESkillMeta :one
SELECT type_id, rank, primary_attr, secondary_attr FROM sde_skill_meta
WHERE type_id = $1;
-- name: ListSDESkillMetaByIDs :many
SELECT type_id, rank, primary_attr, secondary_attr FROM sde_skill_meta
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- name: ListSDERequirementsByType :many
SELECT skill_type_id, level FROM sde_requirements
WHERE type_id = $1
ORDER BY level DESC, skill_type_id;
-- name: ListSDERequirementsByTypes :many
SELECT type_id, skill_type_id, level FROM sde_requirements
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[])
ORDER BY type_id, level DESC, skill_type_id;
-- name: SearchSDESkills :many
SELECT m.type_id, t.name, m.rank
FROM sde_skill_meta m
JOIN sde_types t ON t.type_id = m.type_id
WHERE strpos(lower(t.name), lower($1)) > 0
ORDER BY CASE WHEN strpos(lower(t.name), lower($2)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT 25;

-- Name-to-type-ID lookups for the plan templates (Magic 14 &
-- friends), which name their skills the way the wiki does.
-- name: ListSDETypesByNames :many
SELECT type_id, name FROM sde_types
WHERE name = ANY(sqlc.arg(names)::text[]);
-- name: GetTypeDetail :one
SELECT type_id, description, fetched_at
FROM type_details
WHERE type_id = $1;
-- name: UpsertTypeDetailWant :exec
INSERT INTO type_details (type_id)
VALUES ($1)
ON CONFLICT DO NOTHING;
-- name: SetTypeDetail :exec
INSERT INTO type_details (type_id, description, fetched_at)
VALUES ($1, $2, $3)
ON CONFLICT (type_id) DO UPDATE SET
    description = excluded.description,
    fetched_at  = excluded.fetched_at;
-- name: ListTypeDetailWants :many
SELECT type_id
FROM type_details
WHERE fetched_at IS NULL
ORDER BY type_id
LIMIT sqlc.arg(row_limit)::bigint;
-- name: ListSDEBlueprintsUsingMaterial :many
SELECT b.blueprint_type_id, b.product_type_id, b.product_quantity, m.quantity AS material_quantity
FROM sde_blueprint_materials m
JOIN sde_blueprints b ON b.blueprint_type_id = m.blueprint_type_id
WHERE m.material_type_id = $1
ORDER BY b.product_type_id
LIMIT 50;

-- ---------------------------------------------------------------------
-- Fitting simulator (schema 029): dogma attribute/effect reads for
-- the stat engine. Bulk import stays hand-rolled in the SDE importer
-- alongside the other sde_* tables; reads live here.
-- ---------------------------------------------------------------------
-- name: ListSDETypeAttributes :many
SELECT attribute_id, value FROM sde_type_attributes
WHERE type_id = $1
ORDER BY attribute_id;
-- name: ListSDETypeAttributesByIDs :many
SELECT type_id, attribute_id, value FROM sde_type_attributes
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[])
ORDER BY type_id, attribute_id;
-- name: GetSDEAttributeType :one
SELECT attribute_id, name, stackable, high_is_good, unit_id, default_value FROM sde_attribute_types
WHERE attribute_id = $1;
-- name: ListSDEAttributeTypesByIDs :many
SELECT attribute_id, name, stackable, high_is_good, unit_id, default_value FROM sde_attribute_types
WHERE attribute_id = ANY(sqlc.arg(attribute_ids)::bigint[])
ORDER BY attribute_id;
-- name: ListSDETypeEffects :many
SELECT effect_id, is_default FROM sde_type_effects
WHERE type_id = $1
ORDER BY effect_id;
-- name: ListSDETypeEffectsByIDs :many
SELECT type_id, effect_id, is_default FROM sde_type_effects
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[])
ORDER BY type_id, effect_id;
-- name: GetSDEEffect :one
SELECT effect_id, name, category FROM sde_effects
WHERE effect_id = $1;
-- name: ListSDEEffectsByIDs :many
SELECT effect_id, name, category FROM sde_effects
WHERE effect_id = ANY(sqlc.arg(effect_ids)::bigint[])
ORDER BY effect_id;
-- name: ListSDEEffectModifiers :many
SELECT domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id FROM sde_effect_modifiers
WHERE effect_id = $1
ORDER BY domain, func, modified_attr;
-- name: ListSDEEffectModifiersByIDs :many
SELECT effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id FROM sde_effect_modifiers
WHERE effect_id = ANY(sqlc.arg(effect_ids)::bigint[])
ORDER BY effect_id, domain, func, modified_attr;
-- name: ListSDETypeGroupsByIDs :many
SELECT type_id, group_id FROM sde_types
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[])
ORDER BY type_id;
-- name: ListSDETypePhysicsByIDs :many
SELECT type_id, mass, volume, capacity FROM sde_type_physics
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[])
ORDER BY type_id;
-- name: CountSDETypeAttributes :one
SELECT COUNT(*) FROM sde_type_attributes;
-- name: CountSDEAttributeTypes :one
SELECT COUNT(*) FROM sde_attribute_types;
-- name: CountSDEEffects :one
SELECT COUNT(*) FROM sde_effects;
-- name: CountSDEEffectModifiers :one
SELECT COUNT(*) FROM sde_effect_modifiers;
-- name: CountSDETypeEffects :one
SELECT COUNT(*) FROM sde_type_effects;

-- ---------------------------------------------------------------------
-- Fitting simulator UI: picker feeds for the fit editor.
-- Slot families come from the module's slot effect (dgmEffects,
-- verified against the dump 2026-10-04): 11 loPower, 12 hiPower,
-- 13 medPower, 2663 rigSlot, 3772 subSystem. All reads are local
-- SDE rows; the obtainable floor (published + market group) matches
-- the shared suggestion feed.
-- ---------------------------------------------------------------------
-- name: SuggestSDEShips :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id = 6 AND t.published = 1
  AND strpos(lower(t.name), lower(@q)) > 0
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;
-- name: ListFitSlotTypes :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_type_effects te ON te.type_id = t.type_id AND te.effect_id = @effect_id
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_type_attributes meta ON meta.type_id = t.type_id AND meta.attribute_id = 1692
WHERE t.published = 1 AND t.market_group_id > 0
  AND (@q = '' OR strpos(lower(t.name), lower(@q)) > 0)
  AND (@meta::bigint = 0 OR (@meta::bigint = 1 AND (meta.value IS NULL OR meta.value = 1)) OR (@meta::bigint > 1 AND meta.value = @meta::bigint))
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;
-- name: ListFitDroneTypes :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_type_attributes a ON a.type_id = t.type_id AND a.attribute_id = 1272 AND a.value > 0
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_type_attributes meta ON meta.type_id = t.type_id AND meta.attribute_id = 1692
WHERE t.published = 1 AND t.market_group_id > 0
  AND (@q = '' OR strpos(lower(t.name), lower(@q)) > 0)
  AND (@meta::bigint = 0 OR (@meta::bigint = 1 AND (meta.value IS NULL OR meta.value = 1)) OR (@meta::bigint > 1 AND meta.value = @meta::bigint))
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;
-- name: ListFitChargeTypes :many
SELECT t.type_id, t.name
FROM sde_types t
WHERE t.group_id = ANY(sqlc.arg(group_ids)::bigint[])
  AND t.published = 1 AND t.market_group_id > 0
  AND (@charge_size <= 0 OR EXISTS (
        SELECT 1 FROM sde_type_attributes a
        WHERE a.type_id = t.type_id AND a.attribute_id = 128 AND a.value = @charge_size))
ORDER BY t.name
LIMIT 200;
-- name: GetSDETypeByName :one
SELECT type_id, name FROM sde_types
WHERE lower(name) = lower($1)
ORDER BY published DESC, market_group_id DESC, type_id
LIMIT 1;

-- Which of these types are ships (category 6): the assets a player
-- names. Used to decide whose names are worth asking ESI for.
-- name: ListSDEShipTypeIDs :many
SELECT t.type_id FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id = 6 AND t.type_id = ANY(sqlc.arg(type_ids)::bigint[]);
