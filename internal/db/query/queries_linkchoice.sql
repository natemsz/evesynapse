-- What an account chose at sign-up (schema 048): the activities,
-- features and opted-in enhancements, as JSON, so the next character
-- link opens with them preselected.

-- name: GetLinkChoice :one
SELECT choice FROM account_link_choices WHERE user_id = $1;

-- name: SetLinkChoice :exec
INSERT INTO account_link_choices (user_id, choice, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE SET
    choice = excluded.choice,
    updated_at = excluded.updated_at;
