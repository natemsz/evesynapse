-- Corp pulse change log (pulse_seen / pulse_events, schema
-- 032). The worker diffs stored corp snapshots once a cycle and
-- records what changed; widgets only read.

-- name: UpsertPulseSeen :exec
INSERT INTO pulse_seen (corp_id, kind, key, name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (corp_id, kind, key) DO UPDATE SET name = excluded.name;

-- name: ListPulseSeen :many
SELECT corp_id, kind, key, name FROM pulse_seen WHERE corp_id = $1 AND kind = $2;

-- name: DeletePulseSeen :exec
DELETE FROM pulse_seen WHERE corp_id = $1 AND kind = $2 AND key = $3;

-- name: InsertPulseEvent :exec
INSERT INTO pulse_events (corp_id, kind, key, name, at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING;

-- name: ListPulseEvents :many
SELECT corp_id, kind, key, name, at FROM pulse_events
WHERE corp_id = $1 AND at >= $2 ORDER BY at DESC LIMIT 60;

-- name: PrunePulseEvents :exec
DELETE FROM pulse_events WHERE at < $1;
