-- The bot guard's signing secret: one row, made on first start, so
-- what the guard signs (its cookies) survives a restart.
CREATE TABLE IF NOT EXISTS bot_guard_secrets (
    id         BIGINT PRIMARY KEY CHECK (id = 1),
    secret     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
