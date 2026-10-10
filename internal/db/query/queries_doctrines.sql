-- ---------------------------------------------------------------------
-- Doctrines (schema 026).
-- ---------------------------------------------------------------------

-- name: CreateDoctrine :one
INSERT INTO doctrines (corporation_id, name, category, description, tags, created_by, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
ON CONFLICT (corporation_id, lower(name)) DO NOTHING
RETURNING id;

-- name: UpdateDoctrine :execrows
UPDATE doctrines d SET
    name        = sqlc.arg(name),
    category    = sqlc.arg(category),
    description = sqlc.arg(description),
    tags        = sqlc.arg(tags),
    updated_at  = sqlc.arg(updated_at)
WHERE d.id = sqlc.arg(id)
  AND NOT EXISTS (SELECT 1 FROM doctrines o
                  WHERE o.corporation_id = d.corporation_id AND lower(o.name) = lower(sqlc.arg(name)) AND o.id <> d.id);

-- name: GetDoctrine :one
SELECT * FROM doctrines WHERE id = $1;

-- name: DeleteDoctrine :exec
DELETE FROM doctrines WHERE id = $1;

-- name: ListDoctrinesForCorporations :many
SELECT * FROM doctrines
WHERE corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[])
ORDER BY corporation_id, lower(category), lower(name);

-- name: AddDoctrineFit :one
INSERT INTO doctrine_fits (doctrine_id, name, ship_type_id, items_json, fleet_role, note, added_by, added_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id;

-- name: GetDoctrineFit :one
SELECT f.*, d.corporation_id, d.name AS doctrine_name
FROM doctrine_fits f JOIN doctrines d ON d.id = f.doctrine_id
WHERE f.id = $1;

-- name: DeleteDoctrineFit :exec
DELETE FROM doctrine_fits WHERE id = $1 AND doctrine_id = $2;

-- name: CountDoctrineFits :one
SELECT COUNT(*)::bigint FROM doctrine_fits WHERE doctrine_id = $1;

-- The fits of several doctrines, in the order they were added.
-- name: ListDoctrineFits :many
SELECT * FROM doctrine_fits
WHERE doctrine_id = ANY(sqlc.arg(doctrine_ids)::bigint[])
ORDER BY doctrine_id, id;

-- name: DoctrineHasShip :one
SELECT EXISTS (SELECT 1 FROM doctrine_fits WHERE doctrine_id = $1 AND ship_type_id = $2);

-- Public fits for the fit browser, an account's own among them. A tag
-- matches whole, whatever its case.
-- name: BrowsePublicFittings :many
SELECT lf.id, lf.user_id, lf.name, lf.ship_type_id, lf.items_json, lf.updated_at,
       COALESCE(tn.name, '')::text AS ship_name,
       COALESCE((SELECT c.name FROM characters c WHERE c.user_id = lf.user_id ORDER BY c.character_id LIMIT 1), '')::text AS author_name
FROM local_fittings lf
LEFT JOIN type_names tn ON tn.type_id = lf.ship_type_id
WHERE lf.is_public AND NOT lf.is_draft
  AND (sqlc.arg(q)::text = '' OR lf.name ILIKE '%' || sqlc.arg(q)::text || '%' OR tn.name ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.arg(tag)::text = '' OR EXISTS (
        SELECT 1 FROM jsonb_array_elements_text(
            CASE WHEN jsonb_typeof(lf.items_json::jsonb -> 'tags') = 'array' THEN lf.items_json::jsonb -> 'tags' ELSE '[]'::jsonb END) AS t(tag)
        WHERE lower(t.tag) = lower(sqlc.arg(tag)::text)))
ORDER BY lf.updated_at DESC, lf.id DESC
LIMIT sqlc.arg(row_limit)::bigint;
