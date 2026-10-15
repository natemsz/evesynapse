-- Schema step 048: account_link_choices, what an account chose at sign-up.
--
-- The sign-up asks what the player wants to do and links the character
-- with exactly the permissions that needs. The choice is remembered per
-- account (a small JSON: the activities, the features and the opted-in
-- enhancements) so the next character link opens with it preselected: a
-- confirmation, not a questionnaire. Nothing here grants anything; ESI
-- scopes are what the character's token carries.
CREATE TABLE IF NOT EXISTS account_link_choices (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    choice     TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
