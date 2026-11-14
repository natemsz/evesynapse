-- name: GetBotGuardSecret :one
SELECT secret FROM bot_guard_secrets WHERE id = 1;

-- name: InsertBotGuardSecret :exec
INSERT INTO bot_guard_secrets (id, secret)
VALUES (1, $1)
ON CONFLICT (id) DO NOTHING;
