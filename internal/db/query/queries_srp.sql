-- ---------------------------------------------------------------------
-- Ship replacement (schema 025), and who runs a corporation's
-- programmes.
-- ---------------------------------------------------------------------

-- name: GetCorpPermission :one
SELECT * FROM corp_permissions WHERE corporation_id = $1 AND permission = $2;

-- name: SetCorpPermission :exec
INSERT INTO corp_permissions (corporation_id, permission, kind, ref, updated_by, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (corporation_id, permission) DO UPDATE SET
    kind       = excluded.kind,
    ref        = excluded.ref,
    updated_by = excluded.updated_by,
    updated_at = excluded.updated_at;

-- name: DeleteCorpPermission :exec
DELETE FROM corp_permissions WHERE corporation_id = $1 AND permission = $2;

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

-- A killmail is claimed once: a second request for it creates nothing.
-- name: CreateSRPRequest :one
INSERT INTO srp_requests (corporation_id, op_id, killmail_id, killmail_hash, character_id, user_id,
                          ship_type_id, solar_system_id, lost_at, loss_value, note, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (killmail_id) DO NOTHING
RETURNING id;

-- name: GetSRPRequest :one
SELECT * FROM srp_requests WHERE id = $1;

-- name: ListSRPClaimedKillmails :many
SELECT killmail_id FROM srp_requests WHERE killmail_id = ANY(sqlc.arg(killmail_ids)::bigint[]);

-- name: ListSRPRequestsByUser :many
SELECT * FROM srp_requests WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(row_limit)::bigint;

-- name: ListOpenSRPRequests :many
SELECT * FROM srp_requests
WHERE corporation_id = ANY(sqlc.arg(corporation_ids)::bigint[]) AND status = 'open'
ORDER BY created_at, id;

-- name: ListHandledSRPRequests :many
SELECT * FROM srp_requests
WHERE corporation_id = $1 AND status <> 'open'
ORDER BY handled_at DESC NULLS LAST, id DESC
LIMIT sqlc.arg(row_limit)::bigint;

-- name: SumSRPPaid :one
SELECT COUNT(*)::bigint AS requests, COALESCE(SUM(payout), 0)::double precision AS paid
FROM srp_requests
WHERE corporation_id = $1 AND status = 'paid' AND handled_at >= sqlc.arg(since);

-- A request an account was answered on since a date: what it is told
-- about.
-- name: ListSRPRequestsDecidedForUser :many
SELECT * FROM srp_requests
WHERE user_id = $1 AND status <> 'open' AND handled_at >= sqlc.arg(since)
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

-- A pilot takes back a request nobody has answered.
-- name: WithdrawSRPRequest :execrows
DELETE FROM srp_requests WHERE id = $1 AND user_id = $2 AND status = 'open';

-- name: GetOpAttendance :one
SELECT * FROM op_attendance WHERE op_id = $1 AND character_id = $2;
