-- Communities and verified pilots.

-- name: GetCommunity :one
SELECT * FROM communities WHERE id = $1;

-- name: GetCommunityBySlug :one
SELECT * FROM communities WHERE slug = $1;

-- name: CreateCommunity :one
INSERT INTO communities (slug, name, description, join_policy, listed, invite_token, created_by_character, created_at)
VALUES (sqlc.arg(slug), sqlc.arg(name), sqlc.arg(description), sqlc.arg(join_policy), sqlc.arg(listed),
        sqlc.narg(invite_token), sqlc.arg(created_by_character), sqlc.arg(created_at))
RETURNING *;

-- name: UpdateCommunitySettings :exec
UPDATE communities SET
    name = sqlc.arg(name),
    description = sqlc.arg(description),
    join_policy = sqlc.arg(join_policy),
    listed = sqlc.arg(listed),
    invite_token = sqlc.narg(invite_token)
WHERE id = sqlc.arg(id);

-- name: SetCommunityInvite :exec
UPDATE communities SET invite_token = sqlc.narg(invite_token) WHERE id = sqlc.arg(id);

-- name: ArchiveCommunity :execrows
UPDATE communities SET archived_at = sqlc.arg(archived_at) WHERE id = sqlc.arg(id) AND archived_at IS NULL;

-- name: RestoreCommunity :execrows
UPDATE communities SET archived_at = NULL WHERE id = $1 AND archived_at IS NOT NULL;

-- The directory: listed, open communities, largest first.
-- name: ListDirectoryCommunities :many
SELECT id, slug, name, description, join_policy, member_count
FROM communities
WHERE listed AND archived_at IS NULL
  AND (sqlc.arg(pattern)::text = '' OR name ILIKE sqlc.arg(pattern)::text)
ORDER BY member_count DESC, lower(name), id
LIMIT 100;

-- The communities a set of characters is in or has asked to join, each
-- once, at the highest place any of them holds.
-- name: ListCommunitiesForCharacters :many
SELECT id, slug, name, member_count, pending_count, archived_at, role, state
FROM (
    SELECT DISTINCT ON (c.id) c.id, c.slug, c.name, c.member_count, c.pending_count, c.archived_at, m.role, m.state
    FROM community_members m
    JOIN communities c ON c.id = m.community_id
    WHERE m.character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND m.state <> 'banned'
    ORDER BY c.id,
             CASE m.state WHEN 'active' THEN 0 ELSE 1 END,
             CASE m.role WHEN 'leader' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END
) mine
ORDER BY lower(name), id
LIMIT 100;

-- How many open communities an account's characters lead.
-- name: CountCommunitiesLedByUser :one
SELECT count(*)
FROM community_members m
JOIN communities o ON o.id = m.community_id AND o.archived_at IS NULL
JOIN characters c ON c.character_id = m.character_id
WHERE c.user_id = $1 AND m.role = 'leader';

-- Holds an account's leadership count steady until the transaction ends.
-- name: LockAccountLeadership :exec
SELECT pg_advisory_xact_lock(hashtextextended('communities-led:' || sqlc.arg(user_id)::bigint::text, 0));

-- Serializes changes to who runs a community.
-- name: LockCommunity :one
SELECT id FROM communities WHERE id = $1 FOR UPDATE;

-- name: AddCommunityMember :execrows
INSERT INTO community_members (community_id, character_id, name, role, state, joined_at, added_by)
VALUES (sqlc.arg(community_id), sqlc.arg(character_id), sqlc.arg(name), sqlc.arg(role), sqlc.arg(state),
        sqlc.arg(joined_at), sqlc.arg(added_by))
ON CONFLICT (community_id, character_id) DO NOTHING;

-- name: GetCommunityMember :one
SELECT * FROM community_members WHERE community_id = sqlc.arg(community_id) AND character_id = sqlc.arg(character_id);

-- An active member by exact name, for adding one to a group.
-- name: GetActiveCommunityMemberByName :one
SELECT character_id, name
FROM community_members
WHERE community_id = sqlc.arg(community_id) AND state = 'active' AND lower(name) = lower(sqlc.arg(name)::text)
ORDER BY character_id
LIMIT 1;

-- The rows a set of characters holds in one community.
-- name: ListCommunityMembershipsFor :many
SELECT * FROM community_members
WHERE community_id = sqlc.arg(community_id) AND character_id = ANY(sqlc.arg(character_ids)::bigint[])
ORDER BY lower(name), character_id;

-- One page of a community's members in a state, by name, from a place
-- in the alphabet.
-- name: ListCommunityMembersPage :many
SELECT character_id, name, lower(name)::text AS sort_name, role, state, joined_at
FROM community_members
WHERE community_id = sqlc.arg(community_id) AND state = sqlc.arg(state)
  AND (lower(name), character_id) > (sqlc.arg(after_name)::text, sqlc.arg(after_id)::bigint)
ORDER BY lower(name), character_id
LIMIT sqlc.arg(max_rows);

-- The people who run a community.
-- name: ListCommunityManagers :many
SELECT character_id, name, role
FROM community_members
WHERE community_id = $1 AND state = 'active' AND role IN ('leader', 'admin')
ORDER BY CASE role WHEN 'leader' THEN 0 ELSE 1 END, lower(name), character_id
LIMIT 25;

-- name: GetCommunityLeader :one
SELECT character_id, name FROM community_members WHERE community_id = $1 AND role = 'leader';

-- name: SetCommunityMemberState :execrows
UPDATE community_members SET state = sqlc.arg(state), role = 'member'
WHERE community_id = sqlc.arg(community_id) AND character_id = sqlc.arg(character_id)
  AND state = sqlc.arg(from_state) AND role = sqlc.arg(from_role) AND role <> 'leader';

-- name: SetCommunityMemberRole :execrows
UPDATE community_members SET role = sqlc.arg(role)
WHERE community_id = sqlc.arg(community_id) AND character_id = sqlc.arg(character_id)
  AND state = 'active' AND role = sqlc.arg(from_role);

-- name: DeleteCommunityMember :execrows
DELETE FROM community_members
WHERE community_id = sqlc.arg(community_id) AND character_id = sqlc.arg(character_id)
  AND state = sqlc.arg(state) AND role = sqlc.arg(role) AND role <> 'leader';

-- A leader may step away only from a closed community.
-- name: DeleteClosedCommunityLeader :execrows
DELETE FROM community_members m
USING communities c
WHERE c.id = m.community_id AND c.archived_at IS NOT NULL
  AND m.community_id = sqlc.arg(community_id) AND m.character_id = sqlc.arg(character_id) AND m.role = 'leader';

-- Which of some characters are on a working account and still belong
-- to the person who verified them.
-- name: ListCharactersWithWorkingAccount :many
SELECT c.character_id
FROM characters c
LEFT JOIN verified_pilots v ON v.character_id = c.character_id
WHERE c.character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND c.link_state = 'ok'
  AND (v.owner_hash IS NULL OR v.owner_hash = '' OR c.owner_hash = '' OR v.owner_hash = c.owner_hash);

-- An account's working characters that still belong to who verified
-- them, with the owner hash each joins under.
-- name: ListAccountCharacters :many
SELECT c.character_id, c.name, c.owner_hash
FROM characters c
LEFT JOIN verified_pilots v ON v.character_id = c.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok'
  AND (v.owner_hash IS NULL OR v.owner_hash = '' OR c.owner_hash = '' OR v.owner_hash = c.owner_hash)
ORDER BY lower(c.name), c.character_id;

-- The active memberships of an account's working characters, in open
-- communities, for characters that still belong to who verified them.
-- name: ListCommunityMembershipsByUser :many
SELECT m.community_id, m.character_id, m.role
FROM characters c
JOIN community_members m ON m.character_id = c.character_id AND m.state = 'active'
JOIN communities o ON o.id = m.community_id AND o.archived_at IS NULL
JOIN verified_pilots v ON v.character_id = c.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok'
  AND (v.owner_hash = '' OR c.owner_hash = '' OR v.owner_hash = c.owner_hash);

-- One page of a community group's members, by name.
-- name: ListCommunityGroupMembers :many
SELECT g.character_id, m.name, lower(m.name)::text AS sort_name
FROM org_group_members g
JOIN community_members m ON m.character_id = g.character_id AND m.community_id = sqlc.arg(community_id) AND m.state = 'active'
WHERE g.group_id = sqlc.arg(group_id)
  AND (lower(m.name), g.character_id) > (sqlc.arg(after_name)::text, sqlc.arg(after_id)::bigint)
ORDER BY lower(m.name), g.character_id
LIMIT sqlc.arg(max_rows);

-- Active members of a community not yet in one of its groups.
-- name: ListCommunityGroupCandidates :many
SELECT m.character_id, m.name
FROM community_members m
WHERE m.community_id = sqlc.arg(community_id) AND m.state = 'active'
  AND NOT EXISTS (SELECT 1 FROM org_group_members g WHERE g.group_id = sqlc.arg(group_id) AND g.character_id = m.character_id)
ORDER BY lower(m.name), m.character_id
LIMIT sqlc.arg(max_rows);

-- Which of some characters are active members of a community.
-- name: ListActiveCommunityMembersIn :many
SELECT character_id
FROM community_members
WHERE community_id = sqlc.arg(community_id) AND state = 'active'
  AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: GetVerifiedPilot :one
SELECT * FROM verified_pilots WHERE character_id = $1;

-- Stores a verified identity. An unknown owner hash or affiliation
-- never replaces a known one.
-- name: UpsertVerifiedPilot :exec
INSERT INTO verified_pilots (character_id, name, owner_hash, corporation_id, alliance_id, affiliation_at, verified_at, last_seen_at)
VALUES (sqlc.arg(character_id), sqlc.arg(name), sqlc.arg(owner_hash), sqlc.arg(corporation_id), sqlc.arg(alliance_id),
        sqlc.narg(affiliation_at), sqlc.arg(seen_at), sqlc.arg(seen_at))
ON CONFLICT (character_id) DO UPDATE SET
    name           = excluded.name,
    owner_hash     = CASE WHEN excluded.owner_hash = '' THEN verified_pilots.owner_hash ELSE excluded.owner_hash END,
    corporation_id = CASE WHEN excluded.affiliation_at IS NULL THEN verified_pilots.corporation_id ELSE excluded.corporation_id END,
    alliance_id    = CASE WHEN excluded.affiliation_at IS NULL THEN verified_pilots.alliance_id ELSE excluded.alliance_id END,
    affiliation_at = COALESCE(excluded.affiliation_at, verified_pilots.affiliation_at),
    last_seen_at   = excluded.last_seen_at;

-- name: ReleaseCommunityCharacter :exec
SELECT release_community_character($1);
