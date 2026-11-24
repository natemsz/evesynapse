-- ---------------------------------------------------------------------
-- Fleet invites (schema 177): pilots ready for an invite, and invites
-- queued by those running a fleet and sent by the fleet worker.
-- ---------------------------------------------------------------------

-- name: SetOpSignupReady :execrows
-- A signed-up pilot says they are ready for an invite, or no longer.
UPDATE op_signups SET ready_at = sqlc.narg(ready_at)
WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);

-- name: QueueFleetInvite :exec
-- Asking again clears how the last try went.
INSERT INTO op_invites (op_id, character_id, queued_by, queued_at)
VALUES (sqlc.arg(op_id), sqlc.arg(character_id), sqlc.arg(queued_by), sqlc.arg(queued_at))
ON CONFLICT (op_id, character_id) DO UPDATE SET
    queued_by = excluded.queued_by,
    queued_at = excluded.queued_at,
    sent_at   = NULL,
    failed_at = NULL,
    error     = '';

-- name: ListFleetInvites :many
-- A fleet's invites, for its invite panel; the coming list bounds them.
SELECT character_id, queued_at, sent_at, failed_at, error
FROM op_invites
WHERE op_id = $1
ORDER BY queued_at DESC
LIMIT 300;

-- name: ListPendingFleetInvites :many
-- Invites waiting to go out for fleets in their live window, oldest
-- first, with the fleet as last read and the character it was read
-- through.
SELECT i.op_id, i.character_id, o.capture_boss_id,
       COALESCE((c.payload::jsonb ->> 'fleet_id')::bigint, 0)::bigint AS fleet_id
FROM op_invites i
JOIN ops o ON o.id = i.op_id
LEFT JOIN op_composition c ON c.op_id = i.op_id
WHERE i.sent_at IS NULL AND i.failed_at IS NULL
  AND o.cancelled_at IS NULL
  AND o.starts_at - interval '15 minutes' <= sqlc.arg(now)::timestamptz
  AND o.starts_at + make_interval(mins => (o.duration_minutes + 15)::int) > sqlc.arg(now)::timestamptz
ORDER BY i.queued_at, i.op_id, i.character_id
LIMIT sqlc.arg(max_rows);

-- name: MarkFleetInviteSent :exec
UPDATE op_invites SET sent_at = sqlc.arg(at), error = ''
WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);

-- name: MarkFleetInviteFailed :exec
UPDATE op_invites SET failed_at = sqlc.arg(at), error = sqlc.arg(error)
WHERE op_id = sqlc.arg(op_id) AND character_id = sqlc.arg(character_id);
