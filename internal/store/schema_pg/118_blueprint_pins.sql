-- Blueprint pins (schema 118): the production lines an account
-- has pinned from its Blueprints page, one row per pinned
-- blueprint type. A pin belongs to the account, not to a
-- character: the home Production Lines widget reads the same set
-- whichever character's library the pin was set from.
CREATE TABLE IF NOT EXISTS blueprint_pins (
    user_id           BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    blueprint_type_id BIGINT NOT NULL,
    pinned_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, blueprint_type_id)
);
