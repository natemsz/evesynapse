-- name: ReplaceCorpBridges :exec
-- A corporation's Ansiblex bridges as its latest structures list
-- names them: the ones gone from the list are dropped, the rest
-- written with the corporation's alliance now.
WITH gone AS (
    DELETE FROM alliance_bridges
    WHERE corporation_id = sqlc.arg(corporation_id)
      AND NOT (structure_id = ANY(sqlc.arg(structure_ids)::bigint[]))
)
INSERT INTO alliance_bridges (structure_id, corporation_id, alliance_id, from_system, to_system, name, updated_at)
SELECT unnest(sqlc.arg(structure_ids)::bigint[]), sqlc.arg(corporation_id), sqlc.arg(alliance_id),
       unnest(sqlc.arg(from_systems)::bigint[]), unnest(sqlc.arg(to_systems)::bigint[]),
       unnest(sqlc.arg(names)::text[]), sqlc.arg(updated_at)
ON CONFLICT (structure_id) DO UPDATE SET
    corporation_id = EXCLUDED.corporation_id,
    alliance_id    = EXCLUDED.alliance_id,
    from_system    = EXCLUDED.from_system,
    to_system      = EXCLUDED.to_system,
    name           = EXCLUDED.name,
    updated_at     = EXCLUDED.updated_at;

-- name: ListBridgesForOrgs :many
-- The bridges a reader may see: their alliances' and their
-- corporations', from lists synced since the cutoff. A list that
-- stopped syncing (the director left, the role was taken away)
-- ages out rather than standing forever.
SELECT structure_id, from_system, to_system, name
FROM alliance_bridges
WHERE (alliance_id = ANY(sqlc.arg(alliance_ids)::bigint[]) OR corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[]))
  AND updated_at >= sqlc.arg(since)
ORDER BY structure_id
LIMIT 500;
