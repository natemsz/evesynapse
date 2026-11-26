-- ---------------------------------------------------------------------
-- Doctrines (schema 026).
-- ---------------------------------------------------------------------

-- name: CreateDoctrine :one
INSERT INTO doctrines (corporation_id, owner_kind, owner_id, visibility, name, category, description, tags, created_by, created_at, updated_at)
VALUES (sqlc.arg(corporation_id), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(visibility), sqlc.arg(name), sqlc.arg(category),
        sqlc.arg(description), sqlc.arg(tags), sqlc.arg(created_by), sqlc.arg(created_at), sqlc.arg(created_at))
ON CONFLICT (owner_kind, owner_id, lower(name)) DO NOTHING
RETURNING id;

-- name: UpdateDoctrine :execrows
UPDATE doctrines d SET
    name        = sqlc.arg(name),
    category    = sqlc.arg(category),
    description = sqlc.arg(description),
    tags        = sqlc.arg(tags),
    visibility  = sqlc.arg(visibility),
    updated_at  = sqlc.arg(updated_at)
WHERE d.id = sqlc.arg(id)
  AND NOT EXISTS (SELECT 1 FROM doctrines o
                  WHERE o.owner_kind = d.owner_kind AND o.owner_id = d.owner_id
                    AND lower(o.name) = lower(sqlc.arg(name)) AND o.id <> d.id);

-- name: GetDoctrine :one
SELECT * FROM doctrines WHERE id = $1;

-- name: DeleteDoctrine :exec
DELETE FROM doctrines WHERE id = $1;

-- One owner's doctrines, up to the most a page lists.
-- name: ListDoctrinesForOwner :many
SELECT * FROM doctrines
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
ORDER BY lower(category), lower(name)
LIMIT 200;

-- The doctrines of several owners, for the fitting tool's boosts.
-- name: ListDoctrinesForOwners :many
SELECT * FROM doctrines
WHERE (owner_kind, owner_id) IN (SELECT unnest(sqlc.arg(owner_kinds)::text[]), unnest(sqlc.arg(owner_ids)::bigint[]))
ORDER BY owner_kind, owner_id, lower(category), lower(name)
LIMIT 100;

-- What a community's page lists: its public doctrines, and the rest
-- for its members.
-- name: ListCommunityDoctrines :many
SELECT id, name, category, visibility FROM doctrines
WHERE owner_kind = 'community' AND owner_id = sqlc.arg(community_id)
  AND (visibility = 'public' OR sqlc.arg(include_members)::boolean)
ORDER BY lower(category), lower(name)
LIMIT 24;

-- name: AddDoctrineFit :one
INSERT INTO doctrine_fits (doctrine_id, name, ship_type_id, items_json, fleet_role, note, added_by, added_at, boosts_json)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id;

-- name: GetDoctrineFit :one
SELECT f.*, d.owner_kind, d.owner_id, d.visibility, d.name AS doctrine_name
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

-- name: SetDoctrineFitSearch :exec
-- A fit's search text and own tags, worked out from its items when it
-- is saved (schema 186).
UPDATE doctrine_fits SET search_text = sqlc.arg(search_text), fit_tags = sqlc.arg(fit_tags)
WHERE id = sqlc.arg(id);

-- name: ListDoctrineFitsWithoutSearch :many
-- Fits whose search text is not worked out yet (stored before schema
-- 186, or seeded), oldest first, a bounded batch per worker pass.
SELECT id, items_json FROM doctrine_fits
WHERE search_text IS NULL
ORDER BY id
LIMIT sqlc.arg(row_limit)::bigint;

-- name: DoctrineHasShip :one
SELECT EXISTS (SELECT 1 FROM doctrine_fits WHERE doctrine_id = $1 AND ship_type_id = $2);

-- The fleet boosts of several doctrines' fits, for the fitting tool's
-- doctrine boosts; boosts_json NULL marks a fit not yet worked out.
-- name: ListDoctrineFitBoosts :many
SELECT id, doctrine_id, items_json, boosts_json FROM doctrine_fits
WHERE doctrine_id = ANY(sqlc.arg(doctrine_ids)::bigint[])
ORDER BY doctrine_id, id
LIMIT 400;

-- name: SetDoctrineFitBoosts :exec
UPDATE doctrine_fits SET boosts_json = $2 WHERE id = $1;

-- Stores the doctrine of fleets that have started and have no snapshot
-- yet, a batch at a time; doctrine_id 0 means any doctrine. A doctrine
-- that is gone, or is not the fleet's owner's, is never copied.
-- name: SnapshotStartedFleetDoctrines :execrows
WITH due AS (
    SELECT o.id, o.doctrine_id
    FROM ops o
    JOIN doctrines d ON d.id = o.doctrine_id AND d.owner_kind = o.owner_kind AND d.owner_id = o.owner_id
    WHERE o.doctrine_id <> 0
      AND o.doctrine_snapshot_at IS NULL
      AND o.starts_at <= sqlc.arg(taken_at)::timestamptz
      AND (sqlc.arg(only_doctrine)::bigint = 0 OR o.doctrine_id = sqlc.arg(only_doctrine)::bigint)
    ORDER BY o.starts_at, o.id
    LIMIT 100
    FOR UPDATE OF o
), copied AS (
    SELECT due.id,
           json_build_object(
               'id', d.id, 'name', d.name, 'category', d.category,
               'fits', COALESCE(fits.list, '[]'::json))::text AS payload,
           COALESCE(fits.ships, '{}'::bigint[]) AS ships
    FROM due
    JOIN doctrines d ON d.id = due.doctrine_id
    LEFT JOIN LATERAL (
        SELECT json_agg(json_build_object(
                   'id', f.id, 'name', f.name, 'ship_type_id', f.ship_type_id,
                   'role', f.fleet_role, 'note', f.note, 'items_json', f.items_json) ORDER BY f.id) AS list,
               array_agg(DISTINCT f.ship_type_id) AS ships
        FROM doctrine_fits f WHERE f.doctrine_id = d.id
    ) fits ON TRUE
)
UPDATE ops o SET
    doctrine_snapshot       = copied.payload,
    doctrine_snapshot_ships = copied.ships,
    doctrine_snapshot_at    = sqlc.arg(taken_at)::timestamptz
FROM copied
WHERE o.id = copied.id;

-- An owner's doctrines as its list page filters them: by category, tag,
-- and words in the doctrine or the name or ship of one of its fits.
-- name: ListDoctrinesFiltered :many
SELECT d.* FROM doctrines d
WHERE d.owner_kind = sqlc.arg(owner_kind) AND d.owner_id = sqlc.arg(owner_id)
  AND (sqlc.arg(category)::text = '' OR lower(d.category) = lower(sqlc.arg(category)::text))
  AND (sqlc.arg(tag)::text = '' OR lower(sqlc.arg(tag)::text) = ANY(string_to_array(lower(d.tags), ',')))
  AND (sqlc.arg(needle)::text = ''
       OR position(lower(sqlc.arg(needle)::text) IN lower(concat_ws(E'\n', d.name, d.category, d.description, replace(d.tags, ',', ' ')))) > 0
       OR EXISTS (
           SELECT 1 FROM doctrine_fits f
           LEFT JOIN sde_types t ON t.type_id = f.ship_type_id
           WHERE f.doctrine_id = d.id
             AND (position(lower(sqlc.arg(needle)::text) IN lower(f.name)) > 0
                  OR position(lower(sqlc.arg(needle)::text) IN lower(COALESCE(t.name, ''))) > 0)))
ORDER BY lower(d.category), lower(d.name)
LIMIT sqlc.arg(row_limit);

-- What an owner's doctrine list can be filtered by, in one row.
-- name: GetDoctrineFacets :one
SELECT COUNT(*)::bigint AS total,
       COALESCE(array_agg(DISTINCT d.category) FILTER (WHERE d.category <> ''), '{}')::text[] AS categories,
       COALESCE((SELECT array_agg(DISTINCT tag)
                 FROM doctrines t, unnest(string_to_array(t.tags, ',')) AS tag
                 WHERE t.owner_kind = sqlc.arg(owner_kind) AND t.owner_id = sqlc.arg(owner_id) AND tag <> ''), '{}')::text[] AS tags
FROM doctrines d
WHERE d.owner_kind = sqlc.arg(owner_kind) AND d.owner_id = sqlc.arg(owner_id);

-- Doctrine readiness (schema 183, doctrine_readiness.go). The fold's
-- inputs: the fits of the doctrines some owners keep ("kind:id"
-- keys), and a fingerprint of that fit set that moves when a fit or
-- doctrine is added or removed.
-- name: DoctrineFitsFingerprint :one
SELECT COALESCE(md5(string_agg(f.id::text, ',' ORDER BY f.id)), '')::text AS fits
FROM doctrines d
JOIN doctrine_fits f ON f.doctrine_id = d.id
WHERE (d.owner_kind || ':' || d.owner_id::text) = ANY(sqlc.arg(owner_keys)::text[]);

-- name: ListDoctrineFitsForOwners :many
SELECT f.id, f.doctrine_id, f.ship_type_id, f.items_json
FROM doctrines d
JOIN doctrine_fits f ON f.doctrine_id = d.id
WHERE (d.owner_kind || ':' || d.owner_id::text) = ANY(sqlc.arg(owner_keys)::text[])
ORDER BY f.id
LIMIT 2000;

-- name: GetDoctrineReadinessFold :one
SELECT user_id, inputs, computed_at FROM doctrine_readiness_folds WHERE user_id = $1;

-- name: UpsertDoctrineReadinessFold :exec
INSERT INTO doctrine_readiness_folds (user_id, inputs, computed_at)
VALUES (sqlc.arg(user_id), sqlc.arg(inputs), sqlc.arg(computed_at))
ON CONFLICT (user_id) DO UPDATE SET inputs = EXCLUDED.inputs, computed_at = EXCLUDED.computed_at;

-- name: DeleteDoctrineFitReadiness :exec
DELETE FROM doctrine_fit_readiness WHERE user_id = $1;

-- name: DeleteDoctrineReadiness :exec
DELETE FROM doctrine_readiness WHERE user_id = $1;

-- name: InsertDoctrineFitReadiness :exec
INSERT INTO doctrine_fit_readiness (user_id, fit_id, doctrine_id, ready_ids, near_character, near_missing, near_missing_n, near_sp, computed_at)
VALUES (sqlc.arg(user_id), sqlc.arg(fit_id), sqlc.arg(doctrine_id), sqlc.arg(ready_ids)::bigint[], sqlc.arg(near_character),
        sqlc.arg(near_missing), sqlc.arg(near_missing_n), sqlc.arg(near_sp), sqlc.arg(computed_at))
ON CONFLICT (user_id, fit_id) DO NOTHING;

-- name: InsertDoctrineReadiness :exec
INSERT INTO doctrine_readiness (user_id, doctrine_id, fits, fits_flyable, pilots, pilots_ready, near_character, near_fit, near_missing, near_missing_n, near_sp, computed_at)
VALUES (sqlc.arg(user_id), sqlc.arg(doctrine_id), sqlc.arg(fits), sqlc.arg(fits_flyable), sqlc.arg(pilots), sqlc.arg(pilots_ready),
        sqlc.arg(near_character), sqlc.arg(near_fit), sqlc.arg(near_missing), sqlc.arg(near_missing_n), sqlc.arg(near_sp), sqlc.arg(computed_at))
ON CONFLICT (user_id, doctrine_id) DO NOTHING;

-- An account's doctrines with their readiness, for the home widget:
-- doctrines with the fewest ready pilots first, then by name.
-- name: ListDoctrineReadinessByUser :many
SELECT r.doctrine_id, d.name, d.category, d.owner_kind, d.owner_id, r.fits, r.fits_flyable, r.pilots, r.pilots_ready,
       r.near_character, r.near_fit, COALESCE(nf.name, '')::text AS near_fit_name, COALESCE(nf.ship_type_id, 0)::bigint AS near_ship,
       r.near_missing, r.near_missing_n, r.near_sp, r.computed_at
FROM doctrine_readiness r
JOIN doctrines d ON d.id = r.doctrine_id
LEFT JOIN doctrine_fits nf ON nf.id = r.near_fit
WHERE r.user_id = sqlc.arg(user_id)
ORDER BY r.pilots_ready, lower(d.name), d.id
LIMIT sqlc.arg(row_limit);

-- How many doctrines an account has readiness for, and how many of
-- them a pilot of its can fly: the totals behind a capped list.
-- name: CountDoctrineReadinessByUser :one
SELECT COUNT(*)::bigint AS doctrines,
       (COUNT(*) FILTER (WHERE r.pilots_ready > 0))::bigint AS flyable
FROM doctrine_readiness r
WHERE r.user_id = sqlc.arg(user_id);

-- The fits of some of an account's doctrines with their readiness: at
-- most fits_each per doctrine, so every doctrine asked for gets its
-- first fits however many the others hold.
-- name: ListDoctrineFitReadinessByUser :many
SELECT x.fit_id, x.doctrine_id, x.name, x.ship_type_id, x.fleet_role, x.ready_ids, x.near_character, x.near_missing, x.near_missing_n, x.near_sp
FROM unnest(sqlc.arg(doctrine_ids)::bigint[]) AS d(id)
CROSS JOIN LATERAL (
    SELECT r.fit_id, r.doctrine_id, f.name, f.ship_type_id, f.fleet_role, r.ready_ids, r.near_character, r.near_missing, r.near_missing_n, r.near_sp
    FROM doctrine_fit_readiness r
    JOIN doctrine_fits f ON f.id = r.fit_id
    WHERE r.user_id = sqlc.arg(user_id) AND r.doctrine_id = d.id
    ORDER BY f.id
    LIMIT sqlc.arg(fits_each)::int
) x
ORDER BY x.doctrine_id, x.fit_id
LIMIT 100;
