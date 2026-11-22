-- ---------------------------------------------------------------------
-- Ops (schema 014): EveSynapse's own calendar entries and the
-- sign-ups to them.
-- ---------------------------------------------------------------------

-- The corporation of each of an account's characters.
-- name: ListCharacterCorporationsByUser :many
SELECT c.character_id, c.name, cc.corporation_id
FROM characters c
JOIN character_corporations cc ON cc.character_id = c.character_id
WHERE c.user_id = $1
ORDER BY c.character_id;

-- name: CreateOp :one
INSERT INTO ops (corporation_id, title, description, starts_at, duration_minutes, doctrine, form_up, fc_character_id, created_by_character, created_at, doctrine_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.arg(doctrine_id))
RETURNING id;

-- name: UpdateOp :exec
UPDATE ops SET
    title            = sqlc.arg(title),
    description      = sqlc.arg(description),
    starts_at        = sqlc.arg(starts_at),
    duration_minutes = sqlc.arg(duration_minutes),
    doctrine         = sqlc.arg(doctrine),
    doctrine_id      = sqlc.arg(doctrine_id),
    doctrine_mode    = sqlc.arg(doctrine_mode),
    form_up          = sqlc.arg(form_up),
    fc_character_id  = sqlc.arg(fc_character_id)
WHERE id = sqlc.arg(id);

-- name: GetOp :one
SELECT * FROM ops WHERE id = $1;

-- name: SetOpCancelled :exec
UPDATE ops SET cancelled_at = sqlc.arg(cancelled_at) WHERE id = sqlc.arg(id);

-- name: ListOpsForCorporationsBetween :many
SELECT * FROM ops
WHERE corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[])
  AND starts_at >= sqlc.arg(from_time) AND starts_at < sqlc.arg(to_time)
ORDER BY starts_at, id;

-- name: UpsertOpSignup :exec
INSERT INTO op_signups (op_id, character_id, user_id, response, ship, fleet_role, note, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (op_id, character_id) DO UPDATE SET
    user_id    = excluded.user_id,
    response   = excluded.response,
    ship       = excluded.ship,
    fleet_role = excluded.fleet_role,
    note       = excluded.note,
    updated_at = excluded.updated_at;

-- The first sign-ups to a fleet, up to the most its page shows.
-- name: ListOpSignups :many
SELECT * FROM op_signups WHERE op_id = $1 ORDER BY updated_at, character_id LIMIT 300;

-- name: GetOpSignup :one
SELECT * FROM op_signups WHERE op_id = $1 AND character_id = $2;

-- The answers some characters gave to some fleets.
-- name: ListSignupsOfCharacters :many
SELECT op_id, character_id, response FROM op_signups
WHERE op_id = ANY(sqlc.arg(op_ids)::bigint[])
  AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- ---------------------------------------------------------------------
-- Fleets (schema 165): owners, access rules, commanders, share links.
-- ---------------------------------------------------------------------

-- name: CreateFleet :one
INSERT INTO ops (corporation_id, owner_kind, owner_id, fleet_type, visibility, join_gate, roster_public, share_token,
                 title, description, starts_at, duration_minutes, doctrine, doctrine_id, doctrine_mode, form_up, fc_character_id,
                 created_by_character, created_at)
VALUES (sqlc.arg(corporation_id), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(fleet_type), sqlc.arg(visibility),
        sqlc.arg(join_gate), sqlc.arg(roster_public), sqlc.narg(share_token), sqlc.arg(title), sqlc.arg(description),
        sqlc.arg(starts_at), sqlc.arg(duration_minutes), sqlc.arg(doctrine), sqlc.arg(doctrine_id), sqlc.arg(doctrine_mode), sqlc.arg(form_up),
        sqlc.arg(fc_character_id), sqlc.arg(created_by_character), sqlc.arg(created_at))
RETURNING id;

-- name: UpdateFleetAccess :exec
UPDATE ops SET
    fleet_type    = sqlc.arg(fleet_type),
    visibility    = sqlc.arg(visibility),
    join_gate     = sqlc.arg(join_gate),
    roster_public = sqlc.arg(roster_public)
WHERE id = sqlc.arg(id);

-- name: GetOpByShareToken :one
SELECT * FROM ops WHERE share_token = $1;

-- name: SetOpShareToken :exec
UPDATE ops SET share_token = sqlc.arg(share_token) WHERE id = sqlc.arg(id);

-- Fleets of some owners that have not ended by ends_after and start
-- before starts_before, soonest first.
-- name: ListFleetsForOwners :many
SELECT * FROM ops
WHERE (owner_kind, owner_id) IN (SELECT unnest(sqlc.arg(owner_kinds)::text[]), unnest(sqlc.arg(owner_ids)::bigint[]))
  AND starts_at < sqlc.arg(starts_before)
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY starts_at, id
LIMIT sqlc.arg(row_limit);

-- Fleets some characters have said they will come to, or command.
-- name: ListFleetsForCharacters :many
SELECT * FROM ops
WHERE (id IN (SELECT s.op_id FROM op_signups s WHERE s.character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND s.response <> 'no')
    OR fc_character_id = ANY(sqlc.arg(character_ids)::bigint[])
    OR id IN (SELECT c.op_id FROM op_commanders c WHERE c.character_id = ANY(sqlc.arg(character_ids)::bigint[])))
  AND starts_at < sqlc.arg(starts_before)
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY starts_at, id
LIMIT sqlc.arg(row_limit);

-- name: ListPublicFleets :many
SELECT * FROM ops
WHERE visibility = 'public' AND cancelled_at IS NULL
  AND starts_at < sqlc.arg(starts_before)
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY starts_at, id
LIMIT sqlc.arg(row_limit);

-- Fleets of some owners that have ended, latest first.
-- name: ListPastFleetsForOwners :many
SELECT * FROM ops
WHERE (owner_kind, owner_id) IN (SELECT unnest(sqlc.arg(owner_kinds)::text[]), unnest(sqlc.arg(owner_ids)::bigint[]))
  AND starts_at < sqlc.arg(ended_before)
  AND starts_at + (duration_minutes * interval '1 minute') < sqlc.arg(ended_before)
ORDER BY starts_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListOpAccessRules :many
SELECT * FROM op_access_rules WHERE op_id = $1 ORDER BY action, rule_kind, rule_ref;

-- name: ListOpAccessRulesForOps :many
SELECT * FROM op_access_rules WHERE op_id = ANY(sqlc.arg(op_ids)::bigint[]);

-- name: DeleteOpAccessRules :exec
DELETE FROM op_access_rules WHERE op_id = $1;

-- name: InsertOpAccessRule :exec
INSERT INTO op_access_rules (op_id, action, rule_kind, rule_ref)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: ListOpCommanders :many
SELECT * FROM op_commanders WHERE op_id = $1 ORDER BY added_at, character_id;

-- name: ListOpCommandersForOps :many
SELECT op_id, character_id FROM op_commanders WHERE op_id = ANY(sqlc.arg(op_ids)::bigint[]);

-- name: AddOpCommander :exec
INSERT INTO op_commanders (op_id, character_id, added_by, added_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), sqlc.arg(added_by), sqlc.arg(added_at))
ON CONFLICT (op_id, character_id) DO NOTHING;

-- name: RemoveOpCommander :exec
DELETE FROM op_commanders WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);

-- Which of some characters are linked to a working account: only those
-- can command a fleet.
-- name: ListWorkingCharacterIDs :many
SELECT character_id FROM characters
WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND link_state = 'ok';

-- The stored hand-offs of one programme for some owners.
-- name: ListOwnerPermissionsForOwners :many
SELECT owner_kind, owner_id, kind, ref FROM owner_permissions
WHERE permission = sqlc.arg(permission)
  AND (owner_kind, owner_id) IN (SELECT unnest(sqlc.arg(owner_kinds)::text[]), unnest(sqlc.arg(owner_ids)::bigint[]));

-- The open communities a verified pilot is an active member of.
-- name: ListPilotCommunities :many
SELECT m.community_id FROM community_members m
JOIN communities o ON o.id = m.community_id AND o.archived_at IS NULL
WHERE m.character_id = $1 AND m.state = 'active';

-- The community groups a verified pilot is in, while an active member.
-- name: ListPilotCommunityGroups :many
SELECT g.id FROM org_group_members gm
JOIN org_groups g ON g.id = gm.group_id AND g.owner_kind = 'community'
JOIN community_members m ON m.community_id = g.owner_id AND m.character_id = gm.character_id AND m.state = 'active'
WHERE gm.character_id = $1;

-- ---------------------------------------------------------------------
-- Attendance (schema 015).
-- ---------------------------------------------------------------------

-- Ops whose fleet may be up right now: not cancelled, and inside the
-- window from a little before the start to a little after the end.
-- name: ListOpsToCapture :many
SELECT * FROM ops
WHERE cancelled_at IS NULL
  AND fc_character_id <> 0
  AND starts_at <= sqlc.arg(starts_before)
  AND starts_at >= sqlc.arg(ends_after)::timestamptz - interval '24 hours'
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY capture_checked_at NULLS FIRST, starts_at, id
LIMIT 40;

-- name: SetOpCaptureStatus :exec
UPDATE ops SET capture_status = sqlc.arg(capture_status), capture_checked_at = sqlc.arg(checked_at),
    capture_boss_id = sqlc.arg(boss_id), capture_detail = sqlc.arg(detail)
WHERE id = sqlc.arg(id);

-- Seen in the fleet, everyone in one write. A character already marked
-- by hand becomes a fleet sighting: the fleet is the better witness.
-- name: UpsertFleetAttendance :execrows
INSERT INTO op_attendance (op_id, character_id, ship_type_id, source, first_seen_at, last_seen_at,
    wing_id, squad_id, fleet_position, solar_system_id, suggested_role)
SELECT sqlc.arg(op_id)::bigint, m.character_id, m.ship_type_id, 'fleet',
       sqlc.arg(seen_at)::timestamptz, sqlc.arg(seen_at)::timestamptz,
       m.wing_id, m.squad_id, m.fleet_position, m.solar_system_id, m.suggested_role
FROM (
    SELECT unnest(sqlc.arg(character_ids)::bigint[]) AS character_id,
           unnest(sqlc.arg(ship_type_ids)::bigint[]) AS ship_type_id,
           unnest(sqlc.arg(wing_ids)::bigint[]) AS wing_id,
           unnest(sqlc.arg(squad_ids)::bigint[]) AS squad_id,
           unnest(sqlc.arg(positions)::text[]) AS fleet_position,
           unnest(sqlc.arg(system_ids)::bigint[]) AS solar_system_id,
           unnest(sqlc.arg(suggested_roles)::text[]) AS suggested_role
) AS m
ON CONFLICT (op_id, character_id) DO UPDATE SET
    ship_type_id    = excluded.ship_type_id,
    source          = 'fleet',
    last_seen_at    = excluded.last_seen_at,
    wing_id         = excluded.wing_id,
    squad_id        = excluded.squad_id,
    fleet_position  = excluded.fleet_position,
    solar_system_id = excluded.solar_system_id,
    suggested_role  = excluded.suggested_role;

-- ---------------------------------------------------------------------
-- The live fleet (schema 167).
-- ---------------------------------------------------------------------

-- name: GetOpComposition :one
SELECT op_id, payload, revision, computed_at FROM op_composition WHERE op_id = $1;

-- name: UpsertOpComposition :exec
INSERT INTO op_composition (op_id, payload, revision, computed_at)
VALUES (sqlc.arg(op_id), sqlc.arg(payload), 1, sqlc.arg(computed_at))
ON CONFLICT (op_id) DO UPDATE SET
    payload     = excluded.payload,
    revision    = op_composition.revision + 1,
    computed_at = excluded.computed_at;

-- Stores a commander's change on the composition it was made against.
-- No row changes if the worker wrote a newer one meanwhile; its next
-- read picks the change up from op_member_tags.
-- name: UpdateOpCompositionAt :execrows
UPDATE op_composition SET payload = sqlc.arg(payload), revision = revision + 1
WHERE op_id = sqlc.arg(op_id) AND revision = sqlc.arg(revision);

-- name: ListOpMemberTags :many
SELECT character_id, kind, tag FROM op_member_tags
WHERE op_id = $1
ORDER BY character_id, kind, tag
LIMIT 1500;

-- name: ClearOpMemberTags :exec
DELETE FROM op_member_tags WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);

-- name: InsertOpMemberTag :exec
INSERT INTO op_member_tags (op_id, character_id, kind, tag, set_by, set_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), sqlc.arg(kind), sqlc.arg(tag), sqlc.arg(set_by), sqlc.arg(set_at))
ON CONFLICT DO NOTHING;

-- Marked by hand. Never touches a row the fleet reported.
-- name: InsertManualAttendance :exec
INSERT INTO op_attendance (op_id, character_id, source, first_seen_at, last_seen_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), 'manual', sqlc.arg(seen_at), sqlc.arg(seen_at))
ON CONFLICT (op_id, character_id) DO NOTHING;

-- name: DeleteManualAttendanceExcept :exec
DELETE FROM op_attendance
WHERE op_id = sqlc.arg(op_id) AND source = 'manual'
  AND NOT (character_id = ANY(sqlc.arg(keep_ids)::bigint[]));

-- name: ListOpAttendance :many
SELECT * FROM op_attendance WHERE op_id = $1 ORDER BY character_id;

-- Attendance counts for an owner's fleets since a date, per character:
-- the most active first.
-- name: ListOwnerPAPs :many
SELECT a.character_id,
       COUNT(*)::bigint AS total,
       COUNT(*) FILTER (WHERE o.starts_at >= sqlc.arg(recent_since))::bigint AS recent,
       MAX(o.starts_at)::timestamptz AS last_op
FROM op_attendance a
JOIN ops o ON o.id = a.op_id
WHERE o.owner_kind = sqlc.arg(owner_kind)
  AND o.owner_id = sqlc.arg(owner_id)
  AND o.cancelled_at IS NULL
  AND o.starts_at >= sqlc.arg(since)
GROUP BY a.character_id
ORDER BY total DESC, a.character_id
LIMIT 100;
