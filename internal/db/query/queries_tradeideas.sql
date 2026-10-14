-- Trade ideas for the home page (schema 030): the stored
-- model answer, replaced per refresh. The widget reads this
-- row only; a failed refresh keeps the previous answer.

-- name: GetTradeIdeas :one
SELECT body, as_of, model FROM trade_ideas WHERE id = 1;

-- name: PutTradeIdeas :exec
INSERT INTO trade_ideas (id, body, as_of, model)
VALUES (1, $1, $2, $3)
ON CONFLICT (id) DO UPDATE SET
    body = excluded.body,
    as_of = excluded.as_of,
    model = excluded.model;

-- The last ask and the run of failures it ended (schema 043).

-- name: GetTradeIdeasAttempt :one
SELECT attempted_at, failures FROM trade_ideas_attempts WHERE id = 1;

-- name: PutTradeIdeasAttempt :exec
INSERT INTO trade_ideas_attempts (id, attempted_at, failures)
VALUES (1, $1, $2)
ON CONFLICT (id) DO UPDATE SET
    attempted_at = excluded.attempted_at,
    failures = excluded.failures;
