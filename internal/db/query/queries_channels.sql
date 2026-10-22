-- ---------------------------------------------------------------------
-- Intel channels (schema 078): channels, their grants, their reports.
-- ---------------------------------------------------------------------

-- name: CreateIntelChannel :one
INSERT INTO intel_channels (owner_user_id, name, description, created_at)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(name), sqlc.arg(description), sqlc.arg(created_at))
RETURNING id;

-- name: GetIntelChannel :one
SELECT id, owner_user_id, name, description, created_at FROM intel_channels WHERE id = $1;

-- name: UpdateIntelChannel :exec
UPDATE intel_channels SET name = sqlc.arg(name), description = sqlc.arg(description) WHERE id = sqlc.arg(id);

-- name: DeleteIntelChannel :exec
DELETE FROM intel_channels WHERE id = $1;

-- The channels an account can read: its own, and those granted to any
-- of its characters, their corporations and alliances, or groups they
-- are in. can_post is true for the owner and for any grant that posts.
-- name: ListAccessibleChannels :many
SELECT c.id, c.owner_user_id, c.name, c.description, c.created_at,
       (c.owner_user_id = sqlc.arg(user_id)::bigint OR COALESCE(BOOL_OR(g.can_post), FALSE))::boolean AS can_post
FROM intel_channels c
LEFT JOIN intel_channel_grants g ON g.channel_id = c.id AND (
       (g.kind = 'character'   AND g.target_id = ANY(sqlc.arg(character_ids)::bigint[]))
    OR (g.kind = 'corporation' AND g.target_id = ANY(sqlc.arg(corporation_ids)::bigint[]))
    OR (g.kind = 'alliance'    AND g.target_id = ANY(sqlc.arg(alliance_ids)::bigint[]))
    OR (g.kind = 'group'       AND g.target_id = ANY(sqlc.arg(group_ids)::bigint[])))
GROUP BY c.id
HAVING c.owner_user_id = sqlc.arg(user_id)::bigint OR COUNT(g.channel_id) > 0
ORDER BY lower(c.name), c.id;

-- name: ListIntelChannelGrants :many
SELECT channel_id, kind, target_id, can_post, created_at FROM intel_channel_grants
WHERE channel_id = $1
ORDER BY kind, target_id;

-- name: UpsertIntelChannelGrant :exec
INSERT INTO intel_channel_grants (channel_id, kind, target_id, can_post, created_at)
VALUES (sqlc.arg(channel_id), sqlc.arg(kind), sqlc.arg(target_id), sqlc.arg(can_post), sqlc.arg(created_at))
ON CONFLICT (channel_id, kind, target_id) DO UPDATE SET can_post = EXCLUDED.can_post;

-- name: DeleteIntelChannelGrant :exec
DELETE FROM intel_channel_grants
WHERE channel_id = sqlc.arg(channel_id) AND kind = sqlc.arg(kind) AND target_id = sqlc.arg(target_id);

-- name: CountIntelChannelGrants :one
SELECT COUNT(*) FROM intel_channel_grants WHERE channel_id = $1;

-- name: InsertIntelReport :one
INSERT INTO intel_reports (channel_id, system_id, author_user_id, author_character_id, reporter,
    text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, analysis_pending)
VALUES (sqlc.arg(channel_id), sqlc.arg(system_id), sqlc.arg(author_user_id), sqlc.arg(author_character_id),
    sqlc.arg(reporter), sqlc.arg(text), sqlc.arg(pilots), sqlc.arg(hostiles), sqlc.arg(clear),
    sqlc.arg(scan_id), sqlc.arg(source), sqlc.arg(seen_at), sqlc.arg(created_at),
    sqlc.arg(kind), sqlc.arg(analysis), sqlc.arg(analysis_pending))
RETURNING id;

-- Intel analysis (schema 083).
-- name: ListPendingIntelReports :many
SELECT id, channel_id, system_id, text, hostiles, clear, analysis
FROM intel_reports
WHERE analysis_pending AND created_at >= sqlc.arg(since)::timestamptz
ORDER BY created_at
LIMIT sqlc.arg(max_rows)::bigint;

-- name: SetIntelReportAnalysis :exec
UPDATE intel_reports SET kind = sqlc.arg(kind), analysis = sqlc.arg(analysis), analysis_pending = sqlc.arg(analysis_pending)
WHERE id = sqlc.arg(id);

-- name: GetIntelPilotName :one
SELECT name_lower, character_id, name, checked_at FROM intel_pilot_names WHERE name_lower = $1;

-- name: UpsertIntelPilotName :exec
INSERT INTO intel_pilot_names (name_lower, character_id, name, checked_at)
VALUES (sqlc.arg(name_lower), sqlc.arg(character_id), sqlc.arg(name), sqlc.arg(checked_at))
ON CONFLICT (name_lower) DO UPDATE SET character_id = EXCLUDED.character_id, name = EXCLUDED.name, checked_at = EXCLUDED.checked_at;

-- name: GetIntelPilotAffiliation :one
SELECT character_id, corporation_id, alliance_id, checked_at FROM intel_pilot_affiliations WHERE character_id = $1;

-- name: UpsertIntelPilotAffiliation :exec
INSERT INTO intel_pilot_affiliations (character_id, corporation_id, alliance_id, checked_at)
VALUES (sqlc.arg(character_id), sqlc.arg(corporation_id), sqlc.arg(alliance_id), sqlc.arg(checked_at))
ON CONFLICT (character_id) DO UPDATE SET corporation_id = EXCLUDED.corporation_id, alliance_id = EXCLUDED.alliance_id, checked_at = EXCLUDED.checked_at;

-- The corporations of a channel's recent reporters, and of its
-- owner's characters: the channel's own side, for telling friend from
-- foe by default.
-- name: ListIntelChannelSideCorps :many
SELECT DISTINCT cc.corporation_id
FROM character_corporations cc
WHERE cc.corporation_id > 0 AND cc.character_id IN (
    SELECT r.author_character_id FROM intel_reports r
    WHERE r.channel_id = sqlc.arg(channel_id) AND r.author_character_id > 0 AND r.created_at >= sqlc.arg(since)::timestamptz
    UNION
    SELECT ch.character_id FROM characters ch
    JOIN intel_channels c ON c.owner_user_id = ch.user_id
    WHERE c.id = sqlc.arg(channel_id)
);

-- A channel's reports, latest seen first: a page before a (seen_at,
-- id) key, or everything posted after after_id (the live feed's poll).
-- name: ListIntelReports :many
SELECT id, channel_id, system_id, author_user_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis
FROM intel_reports
WHERE channel_id = sqlc.arg(channel_id)
  AND id > sqlc.arg(after_id)::bigint
  AND (seen_at, id) < (sqlc.arg(before_at)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY seen_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int;

-- The recent reports across some channels: the map's intel layer.
-- name: ListRecentIntelReports :many
SELECT r.id, r.channel_id, r.system_id, r.reporter, r.text, r.pilots, r.hostiles, r.clear, r.scan_id, r.seen_at, r.kind, r.analysis
FROM intel_reports r
WHERE r.channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND r.seen_at >= sqlc.arg(since)::timestamptz
ORDER BY r.seen_at DESC, r.id DESC
LIMIT 2000;

-- name: GetIntelReport :one
SELECT id, channel_id, author_user_id FROM intel_reports WHERE id = $1;

-- name: DeleteIntelReport :exec
DELETE FROM intel_reports WHERE id = $1;

-- name: PruneIntelReports :execrows
DELETE FROM intel_reports WHERE seen_at < sqlc.arg(before)::timestamptz;

-- Whether a report already stands: pasted and watched chat lines are
-- sent again (a restart, two characters in one channel), and the same
-- sighting is stored once.
-- name: IntelReportExists :one
SELECT EXISTS (
    SELECT 1 FROM intel_reports
    WHERE channel_id = sqlc.arg(channel_id) AND seen_at = sqlc.arg(seen_at)
      AND system_id = sqlc.arg(system_id) AND reporter = sqlc.arg(reporter) AND text = sqlc.arg(text)
)::boolean;

-- Posting tokens (schema 079) and Axon pairing (schema 081).
-- name: CreateIntelToken :one
INSERT INTO intel_tokens (user_id, name, prefix, token_hash, created_at, public_key, device)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(prefix), sqlc.arg(token_hash), sqlc.arg(created_at), sqlc.arg(public_key), sqlc.arg(device))
RETURNING id;

-- name: ListIntelTokens :many
SELECT id, name, prefix, created_at, last_used_at, device FROM intel_tokens
WHERE user_id = $1 ORDER BY created_at DESC, id DESC;

-- name: GetIntelTokenByHash :one
SELECT id, user_id, public_key, device FROM intel_tokens WHERE token_hash = $1;

-- name: CreateIntelPairingCode :one
INSERT INTO intel_pairing_codes (user_id, name, code_hash, created_at, expires_at)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(code_hash), sqlc.arg(created_at), sqlc.arg(expires_at))
RETURNING id;

-- name: GetIntelPairingCodeByHash :one
SELECT id, user_id, name, expires_at, used_at FROM intel_pairing_codes WHERE code_hash = $1;

-- name: UseIntelPairingCode :execrows
UPDATE intel_pairing_codes SET used_at = sqlc.arg(at)
WHERE id = sqlc.arg(id) AND used_at IS NULL AND expires_at > sqlc.arg(at);

-- name: TouchIntelToken :exec
UPDATE intel_tokens SET last_used_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: DeleteIntelToken :execrows
DELETE FROM intel_tokens WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: CountIntelTokens :one
SELECT COUNT(*) FROM intel_tokens WHERE user_id = $1;

-- Every system's name: the chat-line reader's dictionary.
-- name: ListSDESystemNames :many
SELECT system_id, name FROM sde_systems;
