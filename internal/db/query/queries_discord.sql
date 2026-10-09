-- ---------------------------------------------------------------------
-- Discord (schema 016): linked accounts, the servers the bot is in
-- and their settings, and what the bot has given.
-- ---------------------------------------------------------------------

-- name: GetDiscordLink :one
SELECT * FROM discord_links WHERE user_id = $1;

-- A Discord account belongs to whoever connected it last: any other
-- account that was linked to it is unlinked first (same transaction).
-- name: DeleteOtherDiscordLinks :exec
DELETE FROM discord_links
WHERE discord_id = sqlc.arg(discord_id) AND user_id <> sqlc.arg(user_id);

-- name: UpsertDiscordLink :exec
INSERT INTO discord_links (user_id, discord_id, username, linked_at)
VALUES (sqlc.arg(user_id), sqlc.arg(discord_id), sqlc.arg(username), sqlc.arg(linked_at))
ON CONFLICT (user_id) DO UPDATE SET
    username = excluded.username,
    linked_at = CASE WHEN discord_links.discord_id = excluded.discord_id THEN discord_links.linked_at ELSE excluded.linked_at END,
    discord_id = excluded.discord_id;

-- name: DeleteDiscordLink :exec
DELETE FROM discord_links WHERE user_id = $1;

-- name: SetDiscordDMNotifications :exec
UPDATE discord_links SET dm_notifications = sqlc.arg(dm_notifications) WHERE user_id = sqlc.arg(user_id);

-- name: ListDiscordLinks :many
SELECT * FROM discord_links ORDER BY user_id;

-- Characters of an account whose link to EVE is in good standing.
-- name: CountLinkedCharactersByUser :one
SELECT count(*) FROM characters WHERE user_id = $1 AND link_state = 'ok';

-- The corporations of the characters of an account whose link to EVE
-- is in good standing: what its Discord roles are worked out from.
-- name: ListLinkedCorporationsByUser :many
SELECT DISTINCT cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok'
ORDER BY cc.corporation_id;

-- Servers.

-- name: GetDiscordGuild :one
SELECT * FROM discord_guilds WHERE guild_id = $1;

-- name: ListDiscordGuilds :many
SELECT * FROM discord_guilds ORDER BY name, guild_id;

-- Adding the bot to a server again keeps the settings it had, unless
-- the server is being claimed for a different owner.
-- name: UpsertDiscordGuild :exec
INSERT INTO discord_guilds (guild_id, name, owner_kind, owner_id, added_by, added_at)
VALUES (sqlc.arg(guild_id), sqlc.arg(name), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(added_by), sqlc.arg(added_at))
ON CONFLICT (guild_id) DO UPDATE SET
    name = excluded.name,
    added_by = excluded.added_by,
    role_linked = CASE WHEN discord_guilds.owner_kind = excluded.owner_kind AND discord_guilds.owner_id = excluded.owner_id THEN discord_guilds.role_linked ELSE '' END,
    role_member = CASE WHEN discord_guilds.owner_kind = excluded.owner_kind AND discord_guilds.owner_id = excluded.owner_id THEN discord_guilds.role_member ELSE '' END,
    ops_channel = CASE WHEN discord_guilds.owner_kind = excluded.owner_kind AND discord_guilds.owner_id = excluded.owner_id THEN discord_guilds.ops_channel ELSE '' END,
    owner_kind = excluded.owner_kind,
    owner_id = excluded.owner_id;

-- name: SetDiscordGuildSettings :exec
UPDATE discord_guilds
SET role_linked = sqlc.arg(role_linked), role_member = sqlc.arg(role_member), ops_channel = sqlc.arg(ops_channel)
WHERE guild_id = sqlc.arg(guild_id);

-- name: DeleteDiscordGuild :exec
DELETE FROM discord_guilds WHERE guild_id = $1;

-- name: GetDiscordOpsShare :one
SELECT * FROM discord_ops_shares WHERE corporation_id = $1;

-- name: SetDiscordOpsShare :exec
INSERT INTO discord_ops_shares (corporation_id, set_by, set_at)
VALUES (sqlc.arg(corporation_id), sqlc.arg(set_by), sqlc.arg(set_at))
ON CONFLICT (corporation_id) DO UPDATE SET set_by = excluded.set_by, set_at = excluded.set_at;

-- name: DeleteDiscordOpsShare :exec
DELETE FROM discord_ops_shares WHERE corporation_id = $1;

-- What the bot has given.

-- name: GetDiscordRoleGrant :one
SELECT * FROM discord_role_grants WHERE discord_id = sqlc.arg(discord_id) AND guild_id = sqlc.arg(guild_id);

-- name: UpsertDiscordRoleGrant :exec
INSERT INTO discord_role_grants (discord_id, guild_id, roles, is_member, checked_at)
VALUES (sqlc.arg(discord_id), sqlc.arg(guild_id), sqlc.arg(roles), sqlc.arg(is_member), sqlc.arg(checked_at))
ON CONFLICT (discord_id, guild_id) DO UPDATE SET roles = excluded.roles, is_member = excluded.is_member, checked_at = excluded.checked_at;

-- name: DeleteDiscordRoleGrant :exec
DELETE FROM discord_role_grants WHERE discord_id = sqlc.arg(discord_id) AND guild_id = sqlc.arg(guild_id);

-- name: ListDiscordRoleGrantsForDiscordID :many
SELECT * FROM discord_role_grants WHERE discord_id = $1 ORDER BY guild_id;

-- name: ListDiscordRoleGrantsForGuild :many
SELECT * FROM discord_role_grants WHERE guild_id = $1 ORDER BY discord_id;

-- Roles given to a Discord account that no EveSynapse account is
-- connected to any more: the account was deleted, disconnected
-- Discord, or connected a different Discord account.
-- name: ListOrphanDiscordRoleGrants :many
SELECT g.* FROM discord_role_grants g
WHERE NOT EXISTS (SELECT 1 FROM discord_links l WHERE l.discord_id = g.discord_id)
ORDER BY g.checked_at
LIMIT sqlc.arg(max_rows);

-- Ops not yet posted to Discord: planned recently, still ahead, not
-- cancelled. The age limit keeps a server that has just been set up
-- from being sent the whole calendar at once.
-- name: ListOpsToAnnounceOnDiscord :many
SELECT * FROM ops
WHERE discord_announced_at IS NULL
  AND cancelled_at IS NULL
  AND starts_at > sqlc.arg(now)
  AND created_at >= sqlc.arg(created_after)
ORDER BY id;

-- name: SetOpDiscordAnnounced :exec
UPDATE ops SET discord_announced_at = sqlc.arg(announced_at) WHERE id = sqlc.arg(id);
