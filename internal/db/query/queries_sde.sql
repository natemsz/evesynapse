-- name: GetTypeName :one
SELECT name FROM type_names
WHERE type_id = $1;
-- name: UpsertTypeName :exec
INSERT INTO type_names (type_id, name)
VALUES ($1, $2)
ON CONFLICT (type_id) DO UPDATE SET
    name = excluded.name;
-- name: ListTypeNamesByExactName :many
SELECT type_id, name FROM type_names
WHERE lower(name) = lower(sqlc.arg(name))
ORDER BY type_id;
-- name: ListTypeNamesByNames :many
SELECT type_id, name FROM type_names
WHERE name = ANY(sqlc.arg(names)::text[])
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

-- name: ListSDETypeNamesByIDs :many
-- The SDE's own names for a batch of types, for reading text
-- that names them (a fused d-scan's rows, above all).
SELECT type_id, name FROM sde_types
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);

-- ---------------------------------------------------------------------
-- SDE static data (schema 003): lookup getters, search, counts, meta.
-- Bulk import inserts are hand-rolled prepared statements inside one
-- transaction in the importer (internal/app/sde.go); reads stay sqlc.
-- ---------------------------------------------------------------------
-- name: GetSDEType :one
SELECT type_id, name, group_id, market_group_id, published, description FROM sde_types
WHERE type_id = $1;

-- name: ListSDETopologyTypes :many
SELECT type_id, name, group_id, market_group_id, published, description FROM sde_types
ORDER BY type_id;
-- The group each of some types is in, by name: a ship's class.
-- name: ListTypeGroupNames :many
SELECT t.type_id, g.name FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- The SDE category of each of some types, batched: type to
-- group to category in one read.
-- name: ListSDETypeCategories :many
SELECT t.type_id, g.category_id FROM sde_types t
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

-- name: ListCelestialNames :many
SELECT item_id, name FROM sde_celestials
WHERE item_id = ANY(sqlc.arg(ids)::bigint[]);
-- name: ListSDEStationsBySystem :many
SELECT station_id, name, system_id FROM sde_stations
WHERE system_id = $1
ORDER BY name;
-- name: GetSDESystem :one
SELECT system_id, name, region_id, security FROM sde_systems
WHERE system_id = $1;
-- name: GetSDESystemByName :one
SELECT system_id, name, region_id, security FROM sde_systems
WHERE LOWER(name) = LOWER($1);
-- name: GetSDERegion :one
SELECT region_id, name FROM sde_regions
WHERE region_id = $1;
-- name: SearchSDESystems :many
SELECT s.system_id, s.name, r.name AS region_name FROM sde_systems s
JOIN sde_regions r ON r.region_id = s.region_id
WHERE s.name ILIKE $1
ORDER BY s.name
LIMIT 10;
-- name: SearchSDERegions :many
SELECT region_id, name FROM sde_regions
WHERE name ILIKE $1
ORDER BY name
LIMIT 10;
-- SuggestSDESystems: systems by name for the search boxes, prefix matches first.
-- name: SuggestSDESystems :many
SELECT s.system_id, s.name, s.region_id, COALESCE(r.name, '') AS region_name, s.security
FROM sde_systems s
LEFT JOIN sde_regions r ON r.region_id = s.region_id
WHERE strpos(lower(s.name), lower(@q::text)) > 0
ORDER BY CASE WHEN strpos(lower(s.name), lower(@q::text)) = 1 THEN 0 ELSE 1 END, s.name
LIMIT @lim::bigint;
-- SuggestSDERegions: regions by name for the search boxes, prefix matches first.
-- name: SuggestSDERegions :many
SELECT region_id, name FROM sde_regions
WHERE strpos(lower(name), lower(@q::text)) > 0
ORDER BY CASE WHEN strpos(lower(name), lower(@q::text)) = 1 THEN 0 ELSE 1 END, name
LIMIT @lim::bigint;
-- name: SearchSDETypes :many
SELECT type_id, name FROM sde_types
WHERE name ILIKE $1
ORDER BY name
LIMIT 20;
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
-- One tree level, branches before leaves: groups that have child
-- groups sort first, name order inside each half, so a level reads
-- as subcategories over contents.
SELECT market_group_id, parent_group_id, name, icon_id, has_types FROM sde_market_groups g
WHERE g.parent_group_id = $1
ORDER BY EXISTS (SELECT 1 FROM sde_market_groups c WHERE c.parent_group_id = g.market_group_id) DESC, name;
-- name: SuggestSDEMarketGroups :many
-- Basket editor group search: market groups whose name mentions
-- the text, prefix matches first, mirroring the type suggest.
SELECT market_group_id, name FROM sde_market_groups
WHERE strpos(lower(name), lower(@q)) > 0
ORDER BY CASE WHEN strpos(lower(name), lower(@q)) = 1 THEN 0 ELSE 1 END, name
LIMIT @lim::bigint;
-- name: CountSDETypesInMarketGroupSubtree :one
-- Every marketable type in a market group and all its descendant
-- groups: the basket editor's bulk add and its size label.
WITH RECURSIVE tree AS (
  SELECT sde_market_groups.market_group_id FROM sde_market_groups WHERE sde_market_groups.market_group_id = $1
  UNION ALL
  SELECT g.market_group_id FROM sde_market_groups g
  JOIN tree t ON g.parent_group_id = t.market_group_id
)
SELECT COUNT(*) FROM sde_types
WHERE sde_types.market_group_id IN (SELECT market_group_id FROM tree)
  AND sde_types.published = 1 AND sde_types.market_group_id > 0;
-- name: ListSDETypesInMarketGroupSubtree :many
WITH RECURSIVE tree AS (
  SELECT sde_market_groups.market_group_id FROM sde_market_groups WHERE sde_market_groups.market_group_id = sqlc.arg(group_id)
  UNION ALL
  SELECT g.market_group_id FROM sde_market_groups g
  JOIN tree t ON g.parent_group_id = t.market_group_id
)
SELECT sde_types.type_id, sde_types.name FROM sde_types
WHERE sde_types.market_group_id IN (SELECT market_group_id FROM tree)
  AND sde_types.published = 1 AND sde_types.market_group_id > 0
ORDER BY sde_types.name
LIMIT sqlc.arg(row_limit)::bigint;
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
-- name: GetSDEOverride :one
SELECT label, archive, sha256, size_bytes, files, uploaded_by, uploaded_at
FROM sde_override
WHERE id = 1;
-- name: GetSDEOverrideInfo :one
SELECT label, sha256, size_bytes, files, uploaded_by, uploaded_at
FROM sde_override
WHERE id = 1;
-- name: PutSDEOverride :exec
INSERT INTO sde_override (id, label, archive, sha256, size_bytes, files, uploaded_by, uploaded_at)
VALUES (1, $1, $2, $3, $4, $5, $6, now())
ON CONFLICT (id) DO UPDATE SET
    label = excluded.label,
    archive = excluded.archive,
    sha256 = excluded.sha256,
    size_bytes = excluded.size_bytes,
    files = excluded.files,
    uploaded_by = excluded.uploaded_by,
    uploaded_at = excluded.uploaded_at;
-- name: DeleteSDEOverride :exec
DELETE FROM sde_override WHERE id = 1;
-- name: DeleteSDEOverrideIfSHA :exec
DELETE FROM sde_override WHERE id = 1 AND sha256 = $1;

-- ---------------------------------------------------------------------
-- Killmail detail store. The worker warms
-- details from the recent-killmails snapshot; pages only read here.
-- ---------------------------------------------------------------------
-- name: GetItemName :one
SELECT name FROM item_names
WHERE item_id = $1;
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
SELECT c.category_id, c.name, COUNT(t.type_id) AS type_count,
       COUNT(t.type_id) FILTER (WHERE t.published = 1) AS published_count
FROM sde_categories c
LEFT JOIN sde_groups g ON g.category_id = c.category_id
LEFT JOIN sde_types t ON t.group_id = g.group_id
GROUP BY c.category_id, c.name
ORDER BY c.name;
-- name: ListSDEGroupsInCategory :many
SELECT g.group_id, g.name, COUNT(t.type_id) AS type_count,
       COUNT(t.type_id) FILTER (WHERE t.published = 1) AS published_count
FROM sde_groups g
LEFT JOIN sde_types t ON t.group_id = g.group_id
WHERE g.category_id = $1
GROUP BY g.group_id, g.name
ORDER BY g.name;
-- name: CountSDETypesInGroup :one
SELECT COUNT(*) FROM sde_types
WHERE group_id = $1 AND strpos(lower(name), lower($2)) > 0;
-- name: ListSDETypesInGroup :many
SELECT type_id, name, market_group_id, published FROM sde_types
WHERE group_id = $1 AND strpos(lower(name), lower($2)) > 0
ORDER BY name
LIMIT sqlc.arg(row_limit)::bigint OFFSET sqlc.arg(row_offset)::bigint;

-- ---------------------------------------------------------------------
-- Next-1 (search & findability): the Items DB global search and
-- the one shared suggestion feed behind every autocomplete box.
-- All local SDE reads; the search filter predicate is the SDE
-- published flag alone, everywhere it appears.
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
  AND (CAST(@published_only AS BIGINT) = 0 OR t.published = 1)
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
  AND (CAST(@published_only AS BIGINT) = 0 OR t.published = 1)
  AND (CAST(@category_id AS BIGINT) = 0 OR g.category_id = @category_id)
  AND (CAST(@group_id AS BIGINT) = 0 OR t.group_id = @group_id);
-- The suggestion floor: published types with a market group.
-- The broad 'all' pool (top banner, Items DB) also admits
-- published blueprint-category types (category 9: blueprints
-- and reaction formulas): they carry no market group because
-- they change hands through contracts alone, and the floor's
-- promise covers items obtainable through contracts too. The
-- narrower pools keep the market floor alone.
-- name: SuggestSDETypesShared :many
SELECT t.type_id, t.name,
       COALESCE(g.name, '') AS group_name,
       COALESCE(c.name, '') AS category_name
FROM sde_types t
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_categories c ON c.category_id = g.category_id
WHERE strpos(lower(t.name), lower(@q)) > 0
  AND t.published = 1
  AND (t.market_group_id > 0
       OR (CAST(@pool AS TEXT) = 'all' AND COALESCE(g.category_id, 0) = 9))
  AND (CAST(@pool AS TEXT) != 'planner' OR EXISTS (
        SELECT 1 FROM sde_blueprints b WHERE b.product_type_id = t.type_id))
  AND (CAST(@pool AS TEXT) != 'skills' OR EXISTS (
        SELECT 1 FROM sde_skill_meta m WHERE m.type_id = t.type_id))
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, lower(t.name), t.type_id
LIMIT @lim::bigint;

-- Pilot-name search for the top banner: ready records whose
-- pilot name field mentions the text. The match runs on
-- the name inside the payload, not the raw payload: a substring
-- match over the whole JSON fills the LIMIT window with rows
-- that merely mention common keys and crowds out real name
-- matches beyond it. The handler still re-checks the name
-- field for ranking before offering a row.
-- name: SearchPilotRecordsByName :many
SELECT character_id, payload
FROM pilot_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(payload::jsonb -> 'profile' ->> 'name'), lower($1)) > 0
ORDER BY character_id
LIMIT 100;

-- Corporation-name search for the top banner and quick jump:
-- same posture as the pilot search above, on the corporation's
-- own name field. Stored payloads come in two shapes (the
-- wrapped record and a bare ESI corporation shared by a peer),
-- so the name is read from whichever shape is present.
-- name: SearchCorporationRecordsByName :many
SELECT corporation_id, payload
FROM corporation_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(COALESCE(payload::jsonb -> 'corp' ->> 'name', payload::jsonb ->> 'name')), lower($1)) > 0
ORDER BY corporation_id
LIMIT 100;

-- Alliance-name search, same posture as the corporation one.
-- name: SearchAllianceRecordsByName :many
SELECT alliance_id, payload
FROM alliance_records
WHERE state = 'ready' AND payload != ''
  AND strpos(lower(COALESCE(payload::jsonb -> 'alliance' ->> 'name', payload::jsonb ->> 'name')), lower($1)) > 0
ORDER BY alliance_id
LIMIT 100;

-- ---------------------------------------------------------------------
-- Search index extraction (search_index.go): whole-table reads a
-- background build folds into the in-memory snapshot the search
-- bar reads, so a keystroke never scans these tables. One pass
-- per build, never per request.
-- ---------------------------------------------------------------------

-- Every published type with the facts the shared suggestion
-- feed's pools filter on.
-- name: ListSearchIndexTypes :many
SELECT t.type_id, t.name,
       COALESCE(g.name, '') AS group_name,
       COALESCE(c.name, '') AS category_name,
       t.market_group_id,
       COALESCE(g.category_id, 0) AS category_id,
       EXISTS (SELECT 1 FROM sde_blueprints b WHERE b.product_type_id = t.type_id) AS has_blueprint,
       EXISTS (SELECT 1 FROM sde_skill_meta m WHERE m.type_id = t.type_id) AS has_skill_meta
FROM sde_types t
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_categories c ON c.category_id = g.category_id
WHERE t.published = 1
ORDER BY t.type_id;

-- name: ListSearchIndexSystems :many
SELECT s.system_id, s.name, s.region_id, COALESCE(r.name, '') AS region_name, s.security
FROM sde_systems s
LEFT JOIN sde_regions r ON r.region_id = s.region_id
ORDER BY s.system_id;

-- name: ListSearchIndexRegions :many
SELECT region_id, name FROM sde_regions
ORDER BY region_id;

-- The ready records, whole: the build decodes each payload's
-- name fields once, where the name searches above would decode
-- per keystroke.
-- name: ListSearchIndexPilotRecords :many
SELECT character_id, payload
FROM pilot_records
WHERE state = 'ready' AND payload != ''
ORDER BY character_id;

-- name: ListSearchIndexCorporationRecords :many
SELECT corporation_id, payload
FROM corporation_records
WHERE state = 'ready' AND payload != ''
ORDER BY corporation_id;

-- name: ListSearchIndexAllianceRecords :many
SELECT alliance_id, payload
FROM alliance_records
WHERE state = 'ready' AND payload != ''
ORDER BY alliance_id;

-- ---------------------------------------------------------------------
-- The daily wallet-history sampler.
-- One row per character per day; the upsert keeps the day's
-- latest values as fresher snapshots land.
-- ---------------------------------------------------------------------
-- name: CountSDEBlueprints :one
SELECT COUNT(*) FROM sde_blueprints;
-- name: ListSDEBlueprintProducts :many
SELECT blueprint_type_id, product_type_id FROM sde_blueprints;
-- name: ListSDEBlueprintProductsForBlueprints :many
SELECT blueprint_type_id, product_type_id FROM sde_blueprints
WHERE blueprint_type_id = ANY(sqlc.arg(blueprint_ids)::bigint[])
ORDER BY blueprint_type_id;
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
-- name: GetSDEBlueprintsForProducts :many
SELECT blueprint_type_id, product_type_id, product_quantity, max_production_limit, manufacturing_time_seconds
FROM sde_blueprints
WHERE product_type_id = ANY(sqlc.arg(product_ids)::bigint[])
ORDER BY product_type_id, blueprint_type_id;
-- name: ListSDEBlueprintMaterialsForBlueprints :many
SELECT blueprint_type_id, material_type_id, quantity FROM sde_blueprint_materials
WHERE blueprint_type_id = ANY(sqlc.arg(blueprint_ids)::bigint[])
ORDER BY blueprint_type_id, material_type_id;
-- name: ListSDEBlueprintSkillsForBlueprints :many
SELECT blueprint_type_id, skill_type_id, level FROM sde_blueprint_skills
WHERE blueprint_type_id = ANY(sqlc.arg(blueprint_ids)::bigint[])
ORDER BY blueprint_type_id, level DESC, skill_type_id;
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
INSERT INTO type_details (type_id, priority, noted_at)
VALUES ($1, sqlc.arg(priority), sqlc.arg(noted_at))
ON CONFLICT (type_id) DO UPDATE SET
    priority = GREATEST(type_details.priority, excluded.priority),
    noted_at = excluded.noted_at;
-- name: SetTypeDetail :exec
INSERT INTO type_details (type_id, description, fetched_at)
VALUES ($1, $2, $3)
ON CONFLICT (type_id) DO UPDATE SET
    description = excluded.description,
    fetched_at  = excluded.fetched_at;
-- name: ListTypeDetailWants :many
-- Typing rows spend their slice highest learned rate first (see
-- ListPilotDrains in queries_market.sql).
SELECT type_id
FROM (
    SELECT type_id, priority, noted_at,
        ROW_NUMBER() OVER (
            PARTITION BY (priority = sqlc.arg(typing_priority)::bigint)
            ORDER BY type_rate DESC NULLS LAST, noted_at DESC NULLS LAST
        ) AS typing_n
    FROM (
        SELECT td.type_id, td.priority, td.noted_at,
            ((COUNT(tg.hit_at) + sqlc.arg(typing_assumed)::float8 * sqlc.arg(typing_weight)::float8)
                / (COUNT(tg.entity_id) + sqlc.arg(typing_weight)::float8)) AS type_rate
        FROM type_details td
        LEFT JOIN typing_guesses tg
            ON tg.kind = 'type_description' AND tg.entity_id = td.type_id
            AND tg.noted_at >= sqlc.arg(typing_since)::timestamptz
        WHERE td.fetched_at IS NULL
        GROUP BY td.type_id, td.priority, td.noted_at
    ) AS rated
) AS wants
WHERE priority != sqlc.arg(typing_priority)::bigint OR typing_n <= sqlc.arg(typing_limit)::bigint
ORDER BY priority DESC,
    CASE WHEN priority = sqlc.arg(typing_priority)::bigint THEN typing_n END ASC NULLS LAST,
    noted_at DESC NULLS LAST, type_id
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
SELECT type_id, mass, volume, capacity, portion_size FROM sde_type_physics
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
-- name: ListSDETypeGroups :many
SELECT t.type_id, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.type_id = ANY(sqlc.arg(type_ids)::bigint[]);

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
-- ListFitFighterTypes: fighters a carrier can launch (squadron size,
-- attribute 2215), without the structure-only squadrons (2740-2742).
-- name: ListFitFighterTypes :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_type_attributes a ON a.type_id = t.type_id AND a.attribute_id = 2215 AND a.value > 0
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_type_attributes meta ON meta.type_id = t.type_id AND meta.attribute_id = 1692
WHERE t.published = 1 AND t.market_group_id > 0
  AND NOT EXISTS (SELECT 1 FROM sde_type_attributes s
                  WHERE s.type_id = t.type_id AND s.attribute_id IN (2740, 2741, 2742) AND s.value > 0)
  AND (@q = '' OR strpos(lower(t.name), lower(@q)) > 0)
  AND (@meta::bigint = 0 OR (@meta::bigint = 1 AND (meta.value IS NULL OR meta.value = 1)) OR (@meta::bigint > 1 AND meta.value = @meta::bigint))
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;
-- ListFitImplantTypes: implants for one slot (implantness, attribute 331).
-- name: ListFitImplantTypes :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_type_attributes a ON a.type_id = t.type_id AND a.attribute_id = 331 AND a.value = @slot::bigint
LEFT JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.published = 1 AND t.market_group_id > 0
  AND (@q = '' OR strpos(lower(t.name), lower(@q)) > 0)
ORDER BY CASE WHEN strpos(lower(t.name), lower(@q)) = 1 THEN 0 ELSE 1 END, t.name
LIMIT @lim::bigint;
-- ListFitResourceImplants: hardwirings that raise a ship's CPU
-- or powergrid output -- the implant solver's ladder. A candidate
-- is an implant type (slot from attribute 331) carrying an effect
-- whose modifier lands on cpuOutput (48) or powerOutput (11); the
-- bonus is the implant's own value of the modifier's attribute
-- (the ladder percent for the output families).
-- name: ListFitResourceImplants :many
SELECT DISTINCT t.type_id, t.name, slot.value::bigint AS slot, mod.modified_attr AS resource_attr, bonus.value AS bonus
FROM sde_types t
JOIN sde_type_attributes slot ON slot.type_id = t.type_id AND slot.attribute_id = 331
JOIN sde_type_effects te ON te.type_id = t.type_id
JOIN sde_effect_modifiers mod ON mod.effect_id = te.effect_id AND mod.modified_attr IN (48, 11)
JOIN sde_type_attributes bonus ON bonus.type_id = t.type_id AND bonus.attribute_id = mod.modifying_attr
WHERE t.published = 1 AND t.market_group_id > 0
ORDER BY mod.modified_attr, bonus.value, t.type_id;
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

-- Every ship type in the SDE, ascending: the true-value
-- pass's evidence-only candidates (ship types with no
-- manufacturing blueprint -- prize hulls and the like -- that
-- only contract evidence can price).
-- name: ListAllSDEShipTypeIDs :many
SELECT t.type_id FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id = 6
ORDER BY t.type_id;

-- Every ship by name with its group, for reading intel reports
-- (intel_analysis.go).
-- name: ListSDEShipNames :many
SELECT t.type_id, t.name, COALESCE(g.name, '') AS group_name
FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id = 6 AND t.published = 1;

-- Every type by name, published or not, for reading intel
-- reports: a type's exact name in free-form text is the thing,
-- never a pilot. Unpublished types are in: the client shows
-- them on scan (a starbase's force field, 16103).
-- name: ListSDETypeNames :many
SELECT t.type_id, t.name
FROM sde_types t
ORDER BY t.published DESC, t.type_id;

-- name: ListSDESystemNeighbours :many
SELECT to_system_id FROM sde_system_jumps WHERE from_system_id = $1;

-- name: ListMarketGroupParentIDs :many
SELECT DISTINCT parent_group_id FROM sde_market_groups
WHERE parent_group_id > 0;

-- The NPC corporation owning a station, for the fee math:
-- the character's standings with the owner and its faction
-- set the broker fee. A station the SDE does not know (a
-- player structure, or a station from before the owners
-- landed) returns no row.
-- name: GetSDEStationOwner :one
SELECT station_id, corporation_id FROM sde_stations WHERE station_id = $1;

-- The faction an NPC corporation belongs to, for the fee
-- math's faction standing. A corporation the SDE does not
-- know returns no row.
-- name: GetSDENPCCorporationFaction :one
SELECT corporation_id, faction_id FROM sde_npc_corporations WHERE corporation_id = $1;

-- ---------------------------------------------------------------------
-- Item sheet (schema 140): one type's shown attributes, bonuses and
-- variation family, each read by the type's key.
-- ---------------------------------------------------------------------
-- name: ListItemSheetAttributes :many
-- The published attributes one type carries, with how the game
-- labels and shows each.
SELECT ta.attribute_id, ta.value, a.name, a.display_name, a.category_id,
       a.unit_id, a.high_is_good, a.display_when_zero,
       COALESCE(u.display_name, '')::text AS unit_name
FROM sde_type_attributes ta
JOIN sde_attribute_types a ON a.attribute_id = ta.attribute_id
LEFT JOIN sde_units u ON u.unit_id = a.unit_id
WHERE ta.type_id = $1 AND a.published = 1
ORDER BY ta.attribute_id;

-- name: ListItemSheetTraits :many
-- One type's bonus lines: per-level skill bonuses first, in skill
-- order, then the role bonus (skill_id -1), then other bonuses (-2).
SELECT t.skill_id, t.bonus, t.bonus_text, t.unit_id,
       COALESCE(u.display_name, '')::text AS unit_name,
       COALESCE(st.name, '')::text AS skill_name
FROM sde_traits t
LEFT JOIN sde_units u ON u.unit_id = t.unit_id
LEFT JOIN sde_types st ON st.type_id = t.skill_id
WHERE t.type_id = $1
ORDER BY (t.skill_id < 0), abs(t.skill_id), t.importance, t.trait_id;

-- name: ListSDEGroupNamesByIDs :many
SELECT group_id, name FROM sde_groups
WHERE group_id = ANY(sqlc.arg(group_ids)::bigint[]);


-- name: ListItemVariationColumns :many
-- The compare columns of the variation family one type is in.
SELECT c.ord, c.attribute_id, a.name, a.display_name, a.unit_id, a.high_is_good,
       COALESCE(u.display_name, '')::text AS unit_name
FROM sde_variations v
JOIN sde_variation_columns c ON c.root_type_id = v.root_type_id
JOIN sde_attribute_types a ON a.attribute_id = c.attribute_id
LEFT JOIN sde_units u ON u.unit_id = a.unit_id
WHERE v.type_id = $1
ORDER BY c.ord;

-- name: HasItemVariations :one
-- Whether one type is in a variation family of two or more.
SELECT EXISTS (SELECT 1 FROM sde_variations WHERE type_id = $1)::bool AS has;

-- name: ListItemVariationRows :many
-- The members of the variation family one type is in, in the game's
-- order (meta level, or strength for an implant or booster ladder),
-- with their column values and tier labels. Families hold at most
-- a few dozen types.
SELECT m.type_id, t.name, m.meta_group_id, m.meta_level,
       COALESCE(NULLIF(m.tier, ''), g.name, '')::text AS meta_group_name, m.vals
FROM sde_variations v
JOIN sde_variations m ON m.root_type_id = v.root_type_id
JOIN sde_types t ON t.type_id = m.type_id
LEFT JOIN sde_meta_groups g ON g.meta_group_id = m.meta_group_id
WHERE v.type_id = $1
ORDER BY m.meta_level, t.name
LIMIT 60;

-- name: ListSDEMetaGroupsByTypes :many
-- Each of some types' meta group (Tech II, faction, deadspace and so
-- on) from the dump's meta types; a type with no row is Tech I.
SELECT type_id, meta_group_id FROM sde_meta_types
WHERE type_id = ANY(sqlc.arg(type_ids)::bigint[]);
-- ListFitBrowseGroups: one level of a family's Browse tree (schema
-- 142); parent 0 is the top.
-- name: ListFitBrowseGroups :many
SELECT market_group_id, name, type_count
FROM fit_browse_groups
WHERE family = @family AND parent_group_id = @parent_group_id
ORDER BY name
LIMIT 200;
-- SearchFitBrowseTypes: one family's Browse types by name (the
-- charge search when no weapon is named), with the picker's meta
-- filter.
-- name: SearchFitBrowseTypes :many
SELECT b.type_id, b.name, COALESCE(g.name, '') AS group_name
FROM fit_browse_types b
JOIN sde_types t ON t.type_id = b.type_id
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_type_attributes meta ON meta.type_id = b.type_id AND meta.attribute_id = 1692
WHERE b.family = @family AND strpos(lower(b.name), lower(@q)) > 0
  AND (@meta::bigint = 0 OR (@meta::bigint = 1 AND (meta.value IS NULL OR meta.value = 1)) OR (@meta::bigint > 1 AND meta.value = @meta::bigint))
ORDER BY CASE WHEN strpos(lower(b.name), lower(@q)) = 1 THEN 0 ELSE 1 END, b.name
LIMIT @lim::bigint;
-- ListFitBrowseTypes: the types a family lists directly in one
-- market group, with the meta filter the picker search uses. The
-- family 'all' lists every family's types in the group.
-- name: ListFitBrowseTypes :many
SELECT b.type_id, b.name, b.family, COALESCE(g.name, '') AS group_name
FROM fit_browse_types b
JOIN sde_types t ON t.type_id = b.type_id
LEFT JOIN sde_groups g ON g.group_id = t.group_id
LEFT JOIN sde_type_attributes meta ON meta.type_id = b.type_id AND meta.attribute_id = 1692
WHERE (@family::text = 'all' OR b.family = @family::text) AND b.market_group_id = @market_group_id
  AND (@meta::bigint = 0 OR (@meta::bigint = 1 AND (meta.value IS NULL OR meta.value = 1)) OR (@meta::bigint > 1 AND meta.value = @meta::bigint))
ORDER BY b.name
LIMIT 200;

-- name: ListSDEDbuffs :many
-- Every buff collection: what a command burst's buff id means. Read
-- once per process and cached (fit.cachedBursts).
SELECT dbuff_id, display_name, aggregate, operation
FROM sde_dbuffs
ORDER BY dbuff_id;

-- name: ListSDEDbuffModifiers :many
-- Every buff's attribute changes, cached with ListSDEDbuffs.
SELECT dbuff_id, kind, attribute_id, group_id, skill_id
FROM sde_dbuff_modifiers
ORDER BY dbuff_id, kind, attribute_id, group_id, skill_id;

-- name: ListBurstBoosters :many
-- Hulls and implants that raise command burst strength (a modifier on
-- warfareBuff1Value, attribute 2469), with the burst skill each one
-- bonuses: the fleet boost picker's booster and mindlink choices.
SELECT DISTINCT t.type_id, t.name, g.category_id, m.skill_type_id
FROM sde_effect_modifiers m
JOIN sde_type_effects te ON te.effect_id = m.effect_id
JOIN sde_types t ON t.type_id = te.type_id AND t.published = 1
JOIN sde_groups g ON g.group_id = t.group_id
WHERE m.modified_attr = 2469 AND m.skill_type_id > 0 AND g.category_id IN (6, 20)
ORDER BY t.name, m.skill_type_id;

-- name: ListBurstTypes :many
-- Command burst modules (group 1770) and the charges they load (the
-- types in the modules' charge groups, attribute 604; some charges
-- carry an old launcher group, so 137 is no guide), with each type's
-- charge group and first required skill (182).
SELECT t.type_id, t.name, t.group_id,
       COALESCE(cg.value, 0)::bigint AS charge_group,
       COALESCE(rs.value, 0)::bigint AS required_skill
FROM sde_types t
LEFT JOIN sde_type_attributes cg ON cg.type_id = t.type_id AND cg.attribute_id = 604
LEFT JOIN sde_type_attributes rs ON rs.type_id = t.type_id AND rs.attribute_id = 182
WHERE t.published = 1
  AND (t.group_id = 1770 OR t.group_id IN (
        SELECT mg.value::bigint FROM sde_type_attributes mg
        JOIN sde_types m ON m.type_id = mg.type_id AND m.group_id = 1770 AND m.published = 1
        WHERE mg.attribute_id = 604))
ORDER BY t.name;
