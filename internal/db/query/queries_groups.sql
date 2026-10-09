-- ---------------------------------------------------------------------
-- Discord role rules and groups (schema 019).
-- ---------------------------------------------------------------------

-- name: ListDiscordRoleRules :many
SELECT * FROM discord_role_rules ORDER BY guild_id, id;

-- name: ListDiscordRoleRulesForGuild :many
SELECT * FROM discord_role_rules WHERE guild_id = $1 ORDER BY id;

-- name: InsertDiscordRoleRule :exec
INSERT INTO discord_role_rules (guild_id, kind, ref, role_id, created_by, created_at)
VALUES (sqlc.arg(guild_id), sqlc.arg(kind), sqlc.arg(ref), sqlc.arg(role_id), sqlc.arg(created_by), sqlc.arg(created_at))
ON CONFLICT (guild_id, kind, ref, role_id) DO NOTHING;

-- name: DeleteDiscordRoleRule :exec
DELETE FROM discord_role_rules WHERE id = sqlc.arg(id) AND guild_id = sqlc.arg(guild_id);

-- name: DeleteDiscordRoleRulesForGuild :exec
DELETE FROM discord_role_rules WHERE guild_id = $1;

-- A group that is deleted takes the rules that named it along.
-- name: DeleteDiscordRoleRulesForGroup :exec
DELETE FROM discord_role_rules WHERE kind = 'group' AND ref = sqlc.arg(group_ref);

-- name: SetDiscordGuildAutoJoin :exec
UPDATE discord_guilds SET auto_join = sqlc.arg(auto_join) WHERE guild_id = sqlc.arg(guild_id);

-- The account's Discord token, sealed; empty strings forget it.
-- name: SetDiscordLinkTokens :exec
UPDATE discord_links
SET access_token = sqlc.arg(access_token), refresh_token = sqlc.arg(refresh_token), token_expiry = sqlc.arg(token_expiry)
WHERE user_id = sqlc.arg(user_id);

-- Forget that an account was looked for in servers and not found, so
-- that it is looked for again at once (it has just connected, and may
-- now be added).
-- name: DeleteDiscordNonMemberGrants :exec
DELETE FROM discord_role_grants WHERE discord_id = $1 AND NOT is_member AND roles = '';

-- name: SetDiscordGuildKick :exec
UPDATE discord_guilds
SET kick_enabled = sqlc.arg(kick_enabled), kick_exempt_roles = sqlc.arg(kick_exempt_roles)
WHERE guild_id = sqlc.arg(guild_id);

-- name: SetDiscordGuildKickChecked :exec
UPDATE discord_guilds SET kick_checked_at = sqlc.arg(kick_checked_at) WHERE guild_id = sqlc.arg(guild_id);

-- name: GetDiscordLinkByDiscordID :one
SELECT * FROM discord_links WHERE discord_id = $1;

-- Channel access (schema 022).

-- name: ListDiscordChannelAccess :many
SELECT * FROM discord_channel_access WHERE guild_id = $1 ORDER BY channel_id, role_id;

-- name: DeleteDiscordChannelAccessForChannel :exec
DELETE FROM discord_channel_access WHERE guild_id = sqlc.arg(guild_id) AND channel_id = sqlc.arg(channel_id);

-- name: InsertDiscordChannelAccess :exec
INSERT INTO discord_channel_access (guild_id, channel_id, role_id, set_by, set_at)
VALUES (sqlc.arg(guild_id), sqlc.arg(channel_id), sqlc.arg(role_id), sqlc.arg(set_by), sqlc.arg(set_at))
ON CONFLICT DO NOTHING;

-- name: SetDiscordGuildOpsChannel :exec
UPDATE discord_guilds SET ops_channel = sqlc.arg(ops_channel) WHERE guild_id = sqlc.arg(guild_id);

-- Groups.

-- name: ListOrgGroupsForOwner :many
SELECT * FROM org_groups
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
ORDER BY lower(name), id;

-- name: GetOrgGroup :one
SELECT * FROM org_groups WHERE id = $1;

-- name: CreateOrgGroup :one
INSERT INTO org_groups (owner_kind, owner_id, name, description, created_by, created_at)
VALUES (sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(name), sqlc.arg(description), sqlc.arg(created_by), sqlc.arg(created_at))
RETURNING id;

-- name: DeleteOrgGroup :exec
DELETE FROM org_groups WHERE id = $1;

-- The members of a group, with the name and corporation EveSynapse
-- has for each character that is linked here. A member that is not
-- linked has no name and no corporation.
-- name: ListOrgGroupMembers :many
SELECT m.character_id, m.added_at,
       COALESCE(c.name, '')::text AS name,
       COALESCE(cc.corporation_id, 0)::bigint AS corporation_id
FROM org_group_members m
LEFT JOIN characters c ON c.character_id = m.character_id
LEFT JOIN character_corporations cc ON cc.character_id = m.character_id
WHERE m.group_id = $1
ORDER BY lower(COALESCE(c.name, '')), m.character_id;

-- name: AddOrgGroupMember :exec
INSERT INTO org_group_members (group_id, character_id, added_by, added_at)
VALUES (sqlc.arg(group_id), sqlc.arg(character_id), sqlc.arg(added_by), sqlc.arg(added_at))
ON CONFLICT DO NOTHING;

-- name: RemoveOrgGroupMember :exec
DELETE FROM org_group_members WHERE group_id = sqlc.arg(group_id) AND character_id = sqlc.arg(character_id);

-- The groups each working character of an account is in.
-- name: ListOrgGroupsByUser :many
SELECT m.group_id, m.character_id, g.owner_kind, g.owner_id
FROM org_group_members m
JOIN org_groups g ON g.id = m.group_id
JOIN characters c ON c.character_id = m.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok';

-- The working characters of an account, each with its corporation.
-- name: ListWorkingCharacterCorporationsByUser :many
SELECT c.character_id, c.name, cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok'
ORDER BY c.character_id;

-- Every linked character in a set of corporations: who a director can
-- put in a group.
-- name: ListCharactersInCorporations :many
SELECT c.character_id, c.name, cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE cc.corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[])
ORDER BY lower(c.name), c.character_id;

-- The corporations EveSynapse has seen linked characters in.
-- name: ListKnownCorporations :many
SELECT DISTINCT corporation_id FROM character_corporations ORDER BY corporation_id;
