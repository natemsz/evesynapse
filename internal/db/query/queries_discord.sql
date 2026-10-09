-- ---------------------------------------------------------------------
-- Discord links (schema 016): which Discord account an EveSynapse
-- account has connected.
-- ---------------------------------------------------------------------

-- name: GetDiscordLink :one
SELECT * FROM discord_links WHERE user_id = $1;

-- A Discord account belongs to whoever connected it last: any other
-- account's link to it is dropped first (in the same transaction).
-- name: DeleteOtherDiscordLinks :exec
DELETE FROM discord_links
WHERE discord_id = sqlc.arg(discord_id) AND user_id <> sqlc.arg(user_id);

-- Connecting a different Discord account starts its roles afresh.
-- name: UpsertDiscordLink :exec
INSERT INTO discord_links (user_id, discord_id, username, linked_at)
VALUES (sqlc.arg(user_id), sqlc.arg(discord_id), sqlc.arg(username), sqlc.arg(linked_at))
ON CONFLICT (user_id) DO UPDATE SET
    username = excluded.username,
    linked_at = CASE WHEN discord_links.discord_id = excluded.discord_id THEN discord_links.linked_at ELSE excluded.linked_at END,
    roles_applied = CASE WHEN discord_links.discord_id = excluded.discord_id THEN discord_links.roles_applied ELSE '' END,
    roles_synced_at = CASE WHEN discord_links.discord_id = excluded.discord_id THEN discord_links.roles_synced_at ELSE NULL END,
    discord_id = excluded.discord_id;

-- name: DeleteDiscordLink :exec
DELETE FROM discord_links WHERE user_id = $1;

-- name: SetDiscordDMNotifications :exec
UPDATE discord_links SET dm_notifications = sqlc.arg(dm_notifications) WHERE user_id = sqlc.arg(user_id);

-- name: ListDiscordLinks :many
SELECT * FROM discord_links ORDER BY user_id;

-- name: SetDiscordRolesApplied :exec
UPDATE discord_links
SET roles_applied = sqlc.arg(roles_applied), roles_synced_at = sqlc.arg(roles_synced_at)
WHERE user_id = sqlc.arg(user_id);

-- Ops not yet posted to Discord: planned recently, still ahead, not
-- cancelled. The age limit keeps an install that turns the bot on from
-- posting its whole calendar at once.
-- name: ListOpsToAnnounceOnDiscord :many
SELECT * FROM ops
WHERE discord_announced_at IS NULL
  AND cancelled_at IS NULL
  AND starts_at > sqlc.arg(now)
  AND created_at >= sqlc.arg(created_after)
ORDER BY id;

-- name: SetOpDiscordAnnounced :exec
UPDATE ops SET discord_announced_at = sqlc.arg(announced_at) WHERE id = sqlc.arg(id);

-- The corporations of an account's characters whose link to EVE is in
-- good standing: what its Discord roles are worked out from.
-- name: ListLinkedCorporationsByUser :many
SELECT DISTINCT cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE c.user_id = $1 AND c.link_state = 'ok'
ORDER BY cc.corporation_id;

-- Characters of an account whose link to EVE is in good standing.
-- name: CountLinkedCharactersByUser :one
SELECT count(*) FROM characters WHERE user_id = $1 AND link_state = 'ok';

-- name: UpsertDiscordRoleGrant :exec
INSERT INTO discord_role_grants (discord_id, guild_id, roles, updated_at)
VALUES (sqlc.arg(discord_id), sqlc.arg(guild_id), sqlc.arg(roles), sqlc.arg(updated_at))
ON CONFLICT (discord_id, guild_id) DO UPDATE SET roles = excluded.roles, updated_at = excluded.updated_at;

-- name: DeleteDiscordRoleGrant :exec
DELETE FROM discord_role_grants WHERE discord_id = sqlc.arg(discord_id) AND guild_id = sqlc.arg(guild_id);

-- Roles given to a Discord account that no EveSynapse account is
-- connected to any more: the account was deleted, disconnected
-- Discord, or connected a different Discord account.
-- name: ListOrphanDiscordRoleGrants :many
SELECT g.* FROM discord_role_grants g
WHERE NOT EXISTS (SELECT 1 FROM discord_links l WHERE l.discord_id = g.discord_id)
ORDER BY g.updated_at
LIMIT sqlc.arg(max_rows);
