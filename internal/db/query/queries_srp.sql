-- ---------------------------------------------------------------------
-- Ship replacement (schema 025), and who runs a corporation's
-- programmes.
-- ---------------------------------------------------------------------

-- name: GetOwnerPermission :one
SELECT * FROM owner_permissions WHERE owner_kind = $1 AND owner_id = $2 AND permission = $3;

-- name: SetOwnerPermission :exec
INSERT INTO owner_permissions (owner_kind, owner_id, permission, kind, ref, updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (owner_kind, owner_id, permission) DO UPDATE SET
    kind       = excluded.kind,
    ref        = excluded.ref,
    updated_by = excluded.updated_by,
    updated_at = excluded.updated_at;

-- name: DeleteOwnerPermission :exec
DELETE FROM owner_permissions WHERE owner_kind = $1 AND owner_id = $2 AND permission = $3;

-- Clears what an owner handed to a group being deleted. A rule naming
-- a group that is gone grants nobody, but reads as "Nobody else".
-- name: DeleteOwnerPermissionsForGroup :exec
WITH old AS (
    DELETE FROM corp_permissions
    WHERE sqlc.arg(owner_kind)::text = 'corporation' AND corporation_id = sqlc.arg(owner_id)::bigint
      AND kind = 'group' AND ref = sqlc.arg(ref)::text
)
DELETE FROM owner_permissions
WHERE owner_kind = sqlc.arg(owner_kind)::text AND owner_id = sqlc.arg(owner_id)::bigint
  AND kind = 'group' AND ref = sqlc.arg(ref)::text;

-- name: GetSRPSettings :one
SELECT * FROM srp_settings WHERE corporation_id = $1;

-- name: SetSRPSettings :exec
INSERT INTO srp_settings (corporation_id, policy, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (corporation_id) DO UPDATE SET
    policy     = excluded.policy,
    updated_at = excluded.updated_at;

-- The stored killmails among those named whose victim is one of the
-- characters named: a pilot's own losses.
-- name: ListLossDetails :many
SELECT killmail_id, hash, payload FROM killmail_details
WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[])
  AND (payload::jsonb -> 'victim' ->> 'character_id')::bigint = ANY(sqlc.arg(character_ids)::bigint[])
ORDER BY killmail_id DESC;

-- A pilot's losses in the public killboard store, for a pilot who has
-- not granted killmail access.
-- name: ListVictimLossesBetween :many
SELECT killmail_id, hash, killmail_time, system_id, victim_character_id, victim_corporation_id, ship_type_id, value_ours
FROM kills
WHERE victim_character_id = ANY(sqlc.arg(character_ids)::bigint[])
  AND killmail_time >= sqlc.arg(from_time) AND killmail_time <= sqlc.arg(to_time)
ORDER BY killmail_time DESC, killmail_id DESC
LIMIT 50;

-- A killmail is claimed once, across every owner: a second request for
-- it creates nothing. user_id 0 is a pilot with no account.
-- name: CreateSRPRequest :one
INSERT INTO srp_requests (corporation_id, owner_kind, owner_id, policy_id, op_id, killmail_id, killmail_hash,
                          character_id, user_id, ship_type_id, solar_system_id, lost_at, loss_value,
                          suggested_payout, fit_evidence, note, created_at)
VALUES (sqlc.arg(corporation_id), sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.narg(policy_id), sqlc.arg(op_id),
        sqlc.arg(killmail_id), sqlc.arg(killmail_hash), sqlc.arg(character_id), NULLIF(sqlc.arg(user_id)::bigint, 0),
        sqlc.arg(ship_type_id), sqlc.arg(solar_system_id), sqlc.arg(lost_at), sqlc.arg(loss_value),
        sqlc.arg(suggested_payout), sqlc.arg(fit_evidence), sqlc.arg(note), sqlc.arg(created_at))
ON CONFLICT (killmail_id) DO NOTHING
RETURNING id;

-- name: GetSRPRequest :one
SELECT * FROM srp_requests WHERE id = $1;

-- name: ListSRPClaimedKillmails :many
SELECT killmail_id FROM srp_requests WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: ListSRPRequestsByUser :many
SELECT * FROM srp_requests
WHERE user_id = sqlc.arg(user_id)::bigint
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::bigint;

-- name: ListSRPRequestsByUserForOwner :many
SELECT * FROM srp_requests
WHERE user_id = sqlc.arg(user_id)::bigint AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::bigint;

-- What some characters have asked for on one fleet.
-- name: ListSRPRequestsOnOpByCharacters :many
SELECT * FROM srp_requests
WHERE op_id = sqlc.arg(op_id) AND character_id = ANY(sqlc.arg(character_ids)::bigint[])
ORDER BY created_at DESC, id DESC
LIMIT 50;

-- One fleet's ledger.
-- name: ListSRPRequestsOnOp :many
SELECT * FROM srp_requests
WHERE op_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 100;

-- The oldest open requests of one owner: the handler's queue.
-- name: ListOpenSRPRequests :many
SELECT * FROM srp_requests
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id) AND status = 'open'
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::bigint;

-- name: CountOpenSRPRequests :one
SELECT COUNT(*)::bigint AS requests, COALESCE(SUM(suggested_payout), 0)::double precision AS owed
FROM srp_requests
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id) AND status = 'open';

-- The newest open requests of each owner, for telling handlers. One
-- owner's backlog cannot crowd out a new request or another owner's.
-- name: ListNewestOpenSRPRequests :many
SELECT r.* FROM srp_requests r
WHERE r.id IN (
    SELECT n.id
    FROM (SELECT unnest(sqlc.arg(owner_kinds)::text[]) AS kind, unnest(sqlc.arg(owner_ids)::bigint[]) AS id) AS o
    CROSS JOIN LATERAL (
        SELECT s.id FROM srp_requests s
        WHERE s.owner_kind = o.kind AND s.owner_id = o.id AND s.status = 'open'
        ORDER BY s.created_at DESC, s.id DESC
        LIMIT 100
    ) n
)
ORDER BY r.created_at, r.id;

-- name: ListHandledSRPRequests :many
SELECT * FROM srp_requests
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id) AND status <> 'open'
ORDER BY handled_at DESC NULLS LAST, id DESC
LIMIT sqlc.arg(row_limit)::bigint;

-- name: SumSRPPaid :one
SELECT COUNT(*)::bigint AS requests, COALESCE(SUM(payout), 0)::double precision AS paid
FROM srp_requests
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
  AND status = 'paid' AND handled_at >= sqlc.arg(since);

-- A request an account was answered on since a date: what it is told
-- about.
-- name: ListSRPRequestsDecidedForUser :many
SELECT * FROM srp_requests
WHERE id IN (
    SELECT d.id FROM srp_requests d
    WHERE d.user_id = sqlc.arg(user_id)::bigint AND d.status <> 'open' AND d.handled_at >= sqlc.arg(since)
    ORDER BY d.handled_at DESC, d.id DESC
    LIMIT 200
)
ORDER BY handled_at, id;

-- Only an open request is decided, so two handlers cannot both pay one.
-- name: DecideSRPRequest :execrows
UPDATE srp_requests SET
    status       = sqlc.arg(status),
    payout       = sqlc.arg(payout),
    handled_by   = sqlc.arg(handled_by),
    handler_note = sqlc.arg(handler_note),
    handled_at   = sqlc.arg(handled_at)
WHERE id = sqlc.arg(id) AND status = 'open';

-- name: ReopenSRPRequest :execrows
UPDATE srp_requests SET status = 'open', payout = 0, handled_by = 0, handler_note = '', handled_at = NULL
WHERE id = $1 AND status <> 'open';

-- A pilot takes back a request nobody has ever answered. One that was
-- answered and opened again stays, so its killmail cannot be claimed
-- anew after a payment.
-- name: WithdrawSRPRequest :execrows
DELETE FROM srp_requests
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)::bigint AND status = 'open' AND answered_at IS NULL;

-- The same from a fleet's tab, by the characters the session has proved.
-- name: WithdrawFleetSRPRequest :execrows
DELETE FROM srp_requests
WHERE id = sqlc.arg(id) AND op_id = sqlc.arg(op_id) AND character_id = ANY(sqlc.arg(character_ids)::bigint[])
  AND status = 'open' AND answered_at IS NULL;

-- ---------------------------------------------------------------------
-- Replacement policies (schema 168).
-- ---------------------------------------------------------------------

-- name: ListSRPPolicies :many
SELECT * FROM srp_policies
WHERE owner_kind = $1 AND owner_id = $2
ORDER BY is_default DESC, lower(name), id
LIMIT 25;

-- name: GetSRPPolicy :one
SELECT * FROM srp_policies WHERE id = $1;

-- name: GetDefaultSRPPolicy :one
SELECT * FROM srp_policies WHERE owner_kind = $1 AND owner_id = $2 AND is_default;

-- name: CreateSRPPolicy :one
INSERT INTO srp_policies (owner_kind, owner_id, name, mode, percent, max_payout, requires_doctrine,
                          requires_attendance, claim_window_hours, ledger_visibility, policy_text,
                          is_default, updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id;

-- name: UpdateSRPPolicy :execrows
UPDATE srp_policies SET
    name                = sqlc.arg(name),
    mode                = sqlc.arg(mode),
    percent             = sqlc.arg(percent),
    max_payout          = sqlc.arg(max_payout),
    requires_doctrine   = sqlc.arg(requires_doctrine),
    requires_attendance = sqlc.arg(requires_attendance),
    claim_window_hours  = sqlc.arg(claim_window_hours),
    ledger_visibility   = sqlc.arg(ledger_visibility),
    policy_text         = sqlc.arg(policy_text),
    is_default          = sqlc.arg(is_default),
    updated_by          = sqlc.arg(updated_by),
    updated_at          = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id);

-- name: ClearDefaultSRPPolicy :exec
UPDATE srp_policies SET is_default = FALSE
WHERE owner_kind = $1 AND owner_id = $2 AND is_default;

-- name: DeleteSRPPolicy :execrows
DELETE FROM srp_policies WHERE id = $1 AND owner_kind = $2 AND owner_id = $3;

-- name: ListSRPPolicyHulls :many
SELECT h.ship_type_id, h.payout, COALESCE(t.name, '')::text AS name
FROM srp_policy_hulls h
LEFT JOIN sde_types t ON t.type_id = h.ship_type_id
WHERE h.policy_id = $1
ORDER BY name, h.ship_type_id
LIMIT 100;

-- Ships named in a policy's hull list, whatever their capitals.
-- name: ListShipTypesByLowerNames :many
SELECT t.type_id, t.name
FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE g.category_id = 6 AND t.published = 1
  AND lower(t.name) = ANY(sqlc.arg(names)::text[])
LIMIT 100;

-- The modules, rigs and subsystems among some types.
-- name: ListFittableTypeIDs :many
SELECT t.type_id
FROM sde_types t
JOIN sde_groups g ON g.group_id = t.group_id
WHERE t.type_id = ANY(sqlc.arg(type_ids)::bigint[]) AND g.category_id IN (7, 32);

-- Answered requests under a policy whose ledger its members can read.
-- name: ListSRPMemberLedger :many
SELECT r.*
FROM srp_requests r
JOIN srp_policies p ON p.id = r.policy_id
WHERE r.owner_kind = $1 AND r.owner_id = $2 AND r.status <> 'open' AND p.ledger_visibility = 'members'
ORDER BY r.handled_at DESC NULLS LAST, r.id DESC
LIMIT 100;

-- The same for one fleet.
-- name: ListSRPMemberLedgerOnOp :many
SELECT r.*
FROM srp_requests r
JOIN srp_policies p ON p.id = r.policy_id
WHERE r.op_id = $1 AND r.status <> 'open' AND p.ledger_visibility = 'members'
ORDER BY r.handled_at DESC NULLS LAST, r.id DESC
LIMIT 100;

-- name: ListOpsByIDs :many
SELECT * FROM ops WHERE id = ANY(sqlc.arg(ids)::bigint[]);

-- An owner's fleets overlapping a stretch of time, newest first.
-- name: ListFleetsForOwnerNewest :many
SELECT * FROM ops
WHERE owner_kind = sqlc.arg(owner_kind) AND owner_id = sqlc.arg(owner_id)
  AND starts_at < sqlc.arg(starts_before)
  AND starts_at + (duration_minutes * interval '1 minute') >= sqlc.arg(ends_after)
ORDER BY starts_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- The communities some characters are active in.
-- name: ListCommunitiesOfCharacters :many
SELECT DISTINCT community_id
FROM community_members
WHERE character_id = ANY(sqlc.arg(character_ids)::bigint[]) AND state = 'active';

-- Which of some characters are on the attendance of some fleets.
-- name: ListOpAttendanceForPilots :many
SELECT op_id, character_id, source, first_seen_at, last_seen_at
FROM op_attendance
WHERE op_id = ANY(sqlc.arg(op_ids)::bigint[]) AND character_id = ANY(sqlc.arg(character_ids)::bigint[]);

-- name: GetSRPPolicyHull :one
SELECT payout FROM srp_policy_hulls WHERE policy_id = $1 AND ship_type_id = $2;

-- name: DeleteSRPPolicyHulls :exec
DELETE FROM srp_policy_hulls WHERE policy_id = $1;

-- name: InsertSRPPolicyHull :exec
INSERT INTO srp_policy_hulls (policy_id, ship_type_id, payout) VALUES ($1, $2, $3)
ON CONFLICT (policy_id, ship_type_id) DO UPDATE SET payout = excluded.payout;

-- The text a corporation writes in its settings is its default policy's.
-- With no default yet it makes one under new_name, which the caller has
-- checked is free, so no other policy is taken over.
-- name: SetDefaultSRPPolicyText :exec
WITH updated AS (
    UPDATE srp_policies p SET
        policy_text = sqlc.arg(policy_text),
        updated_by  = sqlc.arg(updated_by),
        updated_at  = sqlc.arg(updated_at)
    WHERE p.owner_kind = sqlc.arg(owner_kind) AND p.owner_id = sqlc.arg(owner_id) AND p.is_default
    RETURNING p.id
)
INSERT INTO srp_policies (owner_kind, owner_id, name, policy_text, is_default, updated_by, updated_at)
SELECT sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(new_name), sqlc.arg(policy_text), TRUE, sqlc.arg(updated_by), sqlc.arg(updated_at)
WHERE NOT EXISTS (SELECT 1 FROM updated) AND btrim(sqlc.arg(policy_text)::text) <> ''
ON CONFLICT DO NOTHING;

-- name: SetOpSRPPolicy :exec
UPDATE ops SET srp_policy_id = sqlc.narg(srp_policy_id) WHERE id = sqlc.arg(id);

-- Whether any of some characters is on a fleet's attendance.
-- name: AnyOnOpAttendance :one
SELECT EXISTS (
    SELECT 1 FROM op_attendance
    WHERE op_id = sqlc.arg(op_id) AND character_id = ANY(sqlc.arg(character_ids)::bigint[])
)::boolean;

-- name: GetOpAttendance :one
SELECT * FROM op_attendance WHERE op_id = $1 AND character_id = $2;
