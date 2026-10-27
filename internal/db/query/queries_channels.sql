-- ---------------------------------------------------------------------
-- Intel channels (schema 078): channels, their grants, their reports.
-- ---------------------------------------------------------------------

-- name: CreateIntelChannel :one
INSERT INTO intel_channels (owner_user_id, name, description, created_at, owner_kind, owner_id)
VALUES (sqlc.arg(owner_user_id), sqlc.arg(name), sqlc.arg(description), sqlc.arg(created_at), sqlc.arg(owner_kind), sqlc.arg(owner_id))
RETURNING id;

-- name: GetIntelChannel :one
SELECT id, owner_user_id, name, description, created_at, owner_kind, owner_id FROM intel_channels WHERE id = $1;

-- The channels a corporation or alliance owns (schema 082).
-- name: ListOrgIntelChannels :many
SELECT id, owner_user_id, name, description, created_at, owner_kind, owner_id FROM intel_channels
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
ORDER BY lower(name), id;

-- Every rule grant with its channel: judged in Go against each
-- reader's standing.
-- name: ListIntelChannelRuleGrants :many
SELECT r.channel_id, r.rule_kind, r.rule_ref, r.can_post,
       c.owner_user_id, c.name, c.description, c.created_at, c.owner_kind, c.owner_id
FROM intel_channel_rules r
JOIN intel_channels c ON c.id = r.channel_id
WHERE c.owner_kind <> '';

-- name: ListIntelChannelRules :many
SELECT channel_id, rule_kind, rule_ref, can_post, created_at FROM intel_channel_rules
WHERE channel_id = $1
ORDER BY rule_kind, rule_ref;

-- name: UpsertIntelChannelRule :exec
INSERT INTO intel_channel_rules (channel_id, rule_kind, rule_ref, can_post, created_at)
VALUES (sqlc.arg(channel_id), sqlc.arg(rule_kind), sqlc.arg(rule_ref), sqlc.arg(can_post), sqlc.arg(created_at))
ON CONFLICT (channel_id, rule_kind, rule_ref) DO UPDATE SET can_post = EXCLUDED.can_post;

-- name: DeleteIntelChannelRule :exec
DELETE FROM intel_channel_rules
WHERE channel_id = sqlc.arg(channel_id) AND rule_kind = sqlc.arg(rule_kind) AND rule_ref = sqlc.arg(rule_ref);

-- name: CountIntelChannelRules :one
SELECT COUNT(*) FROM intel_channel_rules WHERE channel_id = $1;

-- name: UpdateIntelChannel :exec
UPDATE intel_channels SET name = sqlc.arg(name), description = sqlc.arg(description) WHERE id = sqlc.arg(id);

-- name: DeleteIntelChannel :exec
DELETE FROM intel_channels WHERE id = $1;

-- The channels an account can read: its own, and those granted to any
-- of its characters, their corporations and alliances, or groups they
-- are in. can_post is true for the owner and for any grant that posts.
-- name: ListAccessibleChannels :many
SELECT c.id, c.owner_user_id, c.name, c.description, c.created_at, c.owner_kind, c.owner_id,
       ((c.owner_kind = '' AND c.owner_user_id = sqlc.arg(user_id)::bigint) OR COALESCE(BOOL_OR(g.can_post), FALSE))::boolean AS can_post
FROM intel_channels c
LEFT JOIN intel_channel_grants g ON g.channel_id = c.id AND (
       (g.kind = 'character'   AND g.target_id = ANY(sqlc.arg(character_ids)::bigint[]))
    OR (g.kind = 'corporation' AND g.target_id = ANY(sqlc.arg(corporation_ids)::bigint[]))
    OR (g.kind = 'alliance'    AND g.target_id = ANY(sqlc.arg(alliance_ids)::bigint[]))
    OR (g.kind = 'group'       AND g.target_id = ANY(sqlc.arg(group_ids)::bigint[])))
GROUP BY c.id
HAVING (c.owner_kind = '' AND c.owner_user_id = sqlc.arg(user_id)::bigint) OR COUNT(g.channel_id) > 0
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
    text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, analysis_pending, group_id)
VALUES (sqlc.arg(channel_id), sqlc.arg(system_id), sqlc.arg(author_user_id), sqlc.arg(author_character_id),
    sqlc.arg(reporter), sqlc.arg(text), sqlc.arg(pilots), sqlc.arg(hostiles), sqlc.arg(clear),
    sqlc.arg(scan_id), sqlc.arg(source), sqlc.arg(seen_at), sqlc.arg(created_at),
    sqlc.arg(kind), sqlc.arg(analysis), sqlc.arg(analysis_pending), sqlc.arg(group_id))
RETURNING id;

-- Intel analysis (schema 083). ListPendingIntelReports also
-- carries the author: re-reading a report as a roster counts the
-- poster the roster names as one of its own.
-- name: ListPendingIntelReports :many
SELECT id, channel_id, system_id, text, hostiles, clear, analysis, scan_id, author_character_id, author_user_id, seen_at
FROM intel_reports
WHERE analysis_pending AND created_at >= sqlc.arg(since)::timestamptz
ORDER BY created_at
LIMIT sqlc.arg(max_rows)::bigint;

-- Settled reports the roster re-read has never been offered (the
-- backfill pass in intel_analysis.go): no scan of their own -- a
-- re-read report carries its reading as one -- and no offer
-- marker (the "rr" flag) in the analysis. The caller's window
-- and limit bound the pass like the pending list above.
-- name: ListStoredRosterCandidates :many
SELECT id, channel_id, system_id, text, hostiles, clear, analysis, scan_id, author_character_id, author_user_id, seen_at
FROM intel_reports
WHERE NOT analysis_pending AND scan_id = ''
  AND created_at >= sqlc.arg(since)::timestamptz AND created_at < sqlc.arg(until)::timestamptz
  AND analysis NOT LIKE '%"rr"%'
ORDER BY created_at
LIMIT sqlc.arg(max_rows)::bigint;

-- The reports a roster re-send might already stand as: same
-- channel and system, seen within the caller's window either
-- way. The caller compares pilot sets in Go -- a roster sent
-- fused, spaced and plain has three texts and one set.
-- name: ListRecentRosterReports :many
SELECT id, text, analysis, scan_id FROM intel_reports
WHERE channel_id = sqlc.arg(channel_id) AND system_id = sqlc.arg(system_id)
  AND id <> sqlc.arg(exclude_id)
  AND seen_at >= sqlc.arg(since)::timestamptz AND seen_at <= sqlc.arg(until)::timestamptz
ORDER BY seen_at DESC
LIMIT 25;

-- name: SetIntelReportAnalysis :exec
UPDATE intel_reports SET kind = sqlc.arg(kind), analysis = sqlc.arg(analysis), analysis_pending = sqlc.arg(analysis_pending)
WHERE id = sqlc.arg(id);

-- A report re-read as a roster (its text had fused on the way in):
-- the headline becomes the reading's sentence and the reading is
-- linked as its scan, as for a pasted scan.
-- name: SetIntelReportRoster :exec
UPDATE intel_reports SET text = sqlc.arg(text), pilots = sqlc.arg(pilots), hostiles = sqlc.arg(hostiles),
    scan_id = sqlc.arg(scan_id), kind = sqlc.arg(kind), analysis = sqlc.arg(analysis), analysis_pending = false
WHERE id = sqlc.arg(id);

-- Every pilot name the site has verified, for segmenting a fused
-- roster against them. The analysis dictionaries load lists the
-- same way (the SDE's ships and types); this is the looked-up
-- names table, read whole and kept.
-- name: ListIntelPilotNames :many
SELECT character_id, name FROM intel_pilot_names WHERE character_id > 0;

-- name: GetIntelPilotName :one
SELECT name_lower, character_id, name, checked_at FROM intel_pilot_names WHERE name_lower = $1;

-- name: ListIntelPilotNamesByLower :many
SELECT name_lower, character_id, name, checked_at FROM intel_pilot_names
WHERE name_lower = ANY(sqlc.arg(name_lowers)::text[]);

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
SELECT id, channel_id, system_id, author_user_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, group_id
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

-- One system's reports across some channels, newest first: the
-- system page's intel section. The caller passes only the channels
-- its reader can access (accessibleChannels), so the access check
-- stays in one place.
-- name: ListIntelReportsBySystem :many
SELECT id, channel_id, system_id, author_user_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis
FROM intel_reports
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND system_id = sqlc.arg(system_id)::bigint
ORDER BY seen_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int;

-- The gate-linked reports about one system: reports filed
-- under another system whose stored reading names this system
-- as its gate (the gate figure is read out of the analysis
-- text; no match reads as 0 and matches nothing). A gate
-- reading is intel about both systems, so the gate's own page
-- lists the far side's reports too. Same access shape as
-- ListIntelReportsBySystem: the caller passes only the
-- channels its reader can access.
-- name: ListIntelReportsByGateSystem :many
SELECT id, channel_id, system_id, author_user_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis
FROM intel_reports
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND system_id <> sqlc.arg(system_id)::bigint
  AND COALESCE(substring(analysis from '"gate":\s*([0-9]+)')::bigint, 0) = sqlc.arg(system_id)::bigint
ORDER BY seen_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int;

-- The feed: reports across some channels, latest seen first -- a
-- page before a (seen_at, id) key, or everything posted after
-- after_id (the live feed's poll). The caller passes only the
-- channels its reader can access (accessibleChannels), so the
-- access check stays in one place.
-- name: ListIntelReportsAcrossChannels :many
SELECT id, channel_id, system_id, author_user_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, group_id
FROM intel_reports
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[])
  AND id > sqlc.arg(after_id)::bigint
  AND (seen_at, id) < (sqlc.arg(before_at)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY seen_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int;

-- name: GetIntelReport :one
SELECT id, channel_id, author_user_id FROM intel_reports WHERE id = $1;

-- A report in full: the workbench page renders from the stored
-- row and its analysis.
-- name: GetIntelReportFull :one
SELECT id, channel_id, system_id, author_user_id, author_character_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, analysis_pending, group_id
FROM intel_reports
WHERE id = $1;

-- The report before one in its channel and system: the diff
-- view's other side.
-- name: GetPreviousIntelReport :one
SELECT id, channel_id, system_id, author_user_id, author_character_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, analysis_pending, group_id
FROM intel_reports
WHERE channel_id = sqlc.arg(channel_id)
  AND system_id = sqlc.arg(system_id)
  AND (seen_at, id) < (sqlc.arg(before_at)::timestamptz, sqlc.arg(before_id)::bigint)
ORDER BY seen_at DESC, id DESC
LIMIT 1;

-- The newest report a scan stands behind, if it was ever posted
-- to a channel: a duplicate paste opens the standing report.
-- name: GetIntelReportByScanID :one
SELECT id, channel_id FROM intel_reports
WHERE scan_id = $1
ORDER BY id DESC
LIMIT 1;

-- name: DeleteIntelReport :exec
DELETE FROM intel_reports WHERE id = $1;

-- name: PruneIntelReports :execrows
DELETE FROM intel_reports WHERE seen_at < sqlc.arg(before)::timestamptz;

-- Intel report groups (schema 087). A new report joins a group
-- when channel and system match and the group's latest activity
-- is inside the caller's window; these are the reports it picks
-- from. The caller weighs overlap and nearness in Go.
-- name: ListGroupCandidateReports :many
SELECT id, group_id, reporter, text, scan_id, kind, analysis, seen_at FROM intel_reports
WHERE channel_id = sqlc.arg(channel_id) AND system_id = sqlc.arg(system_id)
  AND seen_at >= sqlc.arg(since)::timestamptz AND seen_at <= sqlc.arg(until)::timestamptz
ORDER BY seen_at DESC
LIMIT sqlc.arg(max_rows)::int;

-- A founding report is its own group: the insert names the
-- group it computed, and a founder's row is corrected to name
-- itself once its id is known.
-- name: SetIntelReportGroup :exec
UPDATE intel_reports SET group_id = sqlc.arg(group_id) WHERE id = sqlc.arg(id);

-- A group's members, newest first (the caller reverses): the
-- composite is computed from these, and the cap keeps the
-- newest members, whose readings govern the picture.
-- name: ListIntelGroupMembers :many
SELECT id, channel_id, system_id, author_user_id, author_character_id, reporter, text, pilots, hostiles, clear, scan_id, source, seen_at, created_at, kind, analysis, analysis_pending, group_id
FROM intel_reports
WHERE group_id = sqlc.arg(group_id)
ORDER BY seen_at DESC, id DESC
LIMIT sqlc.arg(max_rows)::int;

-- Store a group's composite; the standing composite becomes
-- the previous one the diff view compares against.
-- name: UpsertIntelGroupComposite :exec
INSERT INTO intel_group_composites (group_id, channel_id, system_id, composite, previous, members, computed_at, latest_at)
VALUES (sqlc.arg(group_id), sqlc.arg(channel_id), sqlc.arg(system_id), sqlc.arg(composite), '', sqlc.arg(members), sqlc.arg(computed_at)::timestamptz, sqlc.arg(latest_at)::timestamptz)
ON CONFLICT (group_id) DO UPDATE SET
    latest_at = EXCLUDED.latest_at,
    channel_id = EXCLUDED.channel_id,
    system_id = EXCLUDED.system_id,
    previous = intel_group_composites.composite,
    composite = EXCLUDED.composite,
    members = EXCLUDED.members,
    computed_at = EXCLUDED.computed_at;

-- name: GetIntelGroupComposite :one
SELECT group_id, channel_id, system_id, composite, previous, members, computed_at
FROM intel_group_composites WHERE group_id = $1;

-- Composites for several groups at once: the feed and channel
-- pages fetch a whole page's groups in one read.
-- name: ListIntelGroupComposites :many
SELECT group_id, channel_id, system_id, composite, previous, members, computed_at
FROM intel_group_composites WHERE group_id = ANY(sqlc.arg(group_ids)::bigint[]);

-- name: DeleteIntelGroupComposite :exec
DELETE FROM intel_group_composites WHERE group_id = $1;

-- Composites whose group has no reports left (the retention
-- prune took them all) go with them.
-- name: PruneIntelGroupComposites :execrows
DELETE FROM intel_group_composites
WHERE group_id NOT IN (SELECT group_id FROM intel_reports WHERE group_id > 0);

-- Tracked gangs (schema 089, intel_gangs.go): fusion weighs a
-- channel's open gangs when a group's composite lands, keeps
-- the winner's picture current, and closes gangs whose last
-- sighting has aged past the gang lifetime. Renders look gangs
-- up by the groups on the page.

-- name: ListOpenIntelGangs :many
SELECT id, channel_id, first_seen_at, last_seen_at, last_system_id, sightings, strength, closed_at, snapshot
FROM intel_gangs
WHERE channel_id = sqlc.arg(channel_id) AND closed_at IS NULL
ORDER BY last_seen_at DESC
LIMIT sqlc.arg(max_rows)::int;

-- name: GetIntelGang :one
SELECT id, channel_id, first_seen_at, last_seen_at, last_system_id, sightings, strength, closed_at, snapshot
FROM intel_gangs WHERE id = sqlc.arg(id);

-- name: InsertIntelGang :one
INSERT INTO intel_gangs (channel_id, first_seen_at, last_seen_at, last_system_id, sightings, strength, snapshot)
VALUES (sqlc.arg(channel_id), sqlc.arg(first_seen_at)::timestamptz, sqlc.arg(last_seen_at)::timestamptz,
        sqlc.arg(last_system_id), sqlc.arg(sightings), sqlc.arg(strength), sqlc.arg(snapshot))
RETURNING id;

-- name: UpdateIntelGang :exec
UPDATE intel_gangs SET
    first_seen_at = sqlc.arg(first_seen_at)::timestamptz,
    last_seen_at = sqlc.arg(last_seen_at)::timestamptz,
    last_system_id = sqlc.arg(last_system_id),
    sightings = sqlc.arg(sightings),
    strength = sqlc.arg(strength),
    snapshot = sqlc.arg(snapshot)
WHERE id = sqlc.arg(id);

-- name: CloseIntelGang :exec
UPDATE intel_gangs SET closed_at = sqlc.arg(closed_at)::timestamptz
WHERE id = sqlc.arg(id) AND closed_at IS NULL;

-- A provisional gang (below) is deleted when its only group
-- moves to the gang it actually belongs to: its membership
-- rows cascade away, and the sightings live on in the winner's
-- recomputed picture.
-- name: DeleteIntelGang :exec
DELETE FROM intel_gangs WHERE id = sqlc.arg(id);

-- Gangs whose last sighting is older than the lifetime close
-- where they stand: the closed moment is the sighting plus the
-- lifetime, not whenever the sweep happened to run.
-- name: CloseStaleIntelGangs :execrows
UPDATE intel_gangs SET closed_at = last_seen_at + (sqlc.arg(lifetime_secs)::bigint * interval '1 second')
WHERE channel_id = sqlc.arg(channel_id) AND closed_at IS NULL
  AND last_seen_at < sqlc.arg(before)::timestamptz;

-- name: ListIntelGangGroups :many
SELECT group_id FROM intel_gang_groups WHERE gang_id = sqlc.arg(gang_id)
ORDER BY group_id;

-- name: ListIntelGangGroupsForGangs :many
SELECT gang_id, group_id FROM intel_gang_groups
WHERE gang_id = ANY(sqlc.arg(gang_ids)::bigint[])
ORDER BY gang_id, group_id;

-- name: AddIntelGangGroup :exec
INSERT INTO intel_gang_groups (gang_id, group_id) VALUES (sqlc.arg(gang_id), sqlc.arg(group_id))
ON CONFLICT DO NOTHING;

-- name: DeleteIntelGangGroups :exec
DELETE FROM intel_gang_groups WHERE gang_id = sqlc.arg(gang_id);

-- name: DeleteIntelGangGroupMembership :exec
DELETE FROM intel_gang_groups WHERE group_id = sqlc.arg(group_id);

-- The gangs standing behind several groups at once: the feed
-- and channel pages fetch a whole page's gangs in one read.
-- Every membership of a touched gang comes back, not only the
-- page's own groups, so the gang's Groups list is whole.
-- name: ListIntelGangsByGroups :many
SELECT g.id, g.channel_id, g.first_seen_at, g.last_seen_at, g.last_system_id, g.sightings, g.strength, g.closed_at, g.snapshot, gg.group_id
FROM intel_gangs g
JOIN intel_gang_groups touch ON touch.gang_id = g.id AND touch.group_id = ANY(sqlc.arg(group_ids)::bigint[])
JOIN intel_gang_groups gg ON gg.gang_id = g.id;

-- Gangs whose groups have all been pruned go with them.
-- name: PruneIntelGangs :execrows
DELETE FROM intel_gangs
WHERE id NOT IN (SELECT gang_id FROM intel_gang_groups);

-- Gang claims on groups that no longer stand go first, so
-- PruneIntelGangs can take the gangs they emptied.
-- name: PruneIntelGangGroups :execrows
DELETE FROM intel_gang_groups
WHERE group_id NOT IN (SELECT group_id FROM intel_reports WHERE group_id > 0);

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

-- name: DeleteStaleIntelPairingCodes :execrows
DELETE FROM intel_pairing_codes
WHERE id IN (
    SELECT c.id FROM intel_pairing_codes c
    WHERE c.used_at IS NOT NULL OR c.expires_at < sqlc.arg(now)
    ORDER BY c.id LIMIT sqlc.arg(max_rows)
);

-- name: TouchIntelToken :exec
UPDATE intel_tokens SET last_used_at = sqlc.arg(at) WHERE id = sqlc.arg(id);

-- name: DeleteIntelToken :execrows
DELETE FROM intel_tokens WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: CountIntelTokens :one
SELECT COUNT(*) FROM intel_tokens WHERE user_id = $1;

-- Every system's name: the chat-line reader's dictionary.
-- name: ListSDESystemNames :many
SELECT system_id, name FROM sde_systems;

-- Intel threads (schema 095, intel_threads.go): the groups a new
-- group may join the thread of, newest first -- the channel's
-- groups on the given systems whose latest moment falls inside
-- the window, the group itself left out.
-- name: ListThreadCandidates :many
SELECT group_id, thread_id, system_id, latest_at FROM intel_group_composites
WHERE channel_id = sqlc.arg(channel_id) AND group_id <> sqlc.arg(group_id) AND thread_id > 0
  AND system_id = ANY(sqlc.arg(system_ids)::bigint[])
  AND latest_at >= sqlc.arg(since)::timestamptz AND latest_at <= sqlc.arg(until)::timestamptz
ORDER BY latest_at DESC
LIMIT 20;

-- name: GetIntelGroupThread :one
SELECT thread_id FROM intel_group_composites WHERE group_id = $1;

-- A thread's span: its earliest and latest group moments.
-- name: GetIntelThreadSpan :one
SELECT min(latest_at)::timestamptz AS first_at, max(latest_at)::timestamptz AS last_at
FROM intel_group_composites WHERE thread_id = $1;

-- A group's thread is set once, when it is first stored.
-- name: SetIntelGroupThread :exec
UPDATE intel_group_composites SET thread_id = sqlc.arg(thread_id)
WHERE group_id = sqlc.arg(group_id) AND thread_id = 0;

-- A thread's groups, newest first.
-- name: ListIntelThreadGroups :many
SELECT group_id, system_id, latest_at FROM intel_group_composites
WHERE thread_id = $1 ORDER BY latest_at DESC;

-- The intel dashboard's rows (intel_dashboard.go): each recent
-- thread in the given channels, by its newest group.
-- name: ListRecentThreads :many
SELECT DISTINCT ON (thread_id) thread_id, group_id, channel_id, system_id, latest_at, composite
FROM intel_group_composites
WHERE channel_id = ANY(sqlc.arg(channel_ids)::bigint[]) AND thread_id > 0
  AND latest_at >= sqlc.arg(since)::timestamptz
ORDER BY thread_id, latest_at DESC;

-- The systems of several threads' groups, for the rows' system
-- counts.
-- name: ListThreadsSystems :many
SELECT thread_id, system_id FROM intel_group_composites
WHERE thread_id = ANY(sqlc.arg(thread_ids)::bigint[]);
