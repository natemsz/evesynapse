-- Schema step 017: one Discord bot in many servers.
--
-- Step 016 kept one set of Discord settings for the whole install, in
-- its environment. The bot is now added to any number of servers, and
-- each has its own settings, chosen on the site by the directors of
-- the corporation or alliance it belongs to.
--
-- discord_guilds: the servers the bot has been added to through this
-- site. A server belongs to one corporation or one alliance (its
-- owner): the members of the owner are who the member role is for,
-- and the directors of the owner are who may change this row.
--
--   role_linked  given to anyone in the server who has connected an
--                EveSynapse account with a working character
--   role_member  given to those of them with a character in the
--                owner corporation, or in a corporation of the owner
--                alliance
--   ops_channel  where new ops of the owner are posted
--
-- Empty means "do not".

CREATE TABLE IF NOT EXISTS discord_guilds (
    guild_id    TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    owner_kind  TEXT NOT NULL CHECK (owner_kind IN ('corporation', 'alliance')),
    owner_id    BIGINT NOT NULL,
    added_by    BIGINT NOT NULL,
    added_at    timestamptz NOT NULL DEFAULT now(),
    role_linked TEXT NOT NULL DEFAULT '',
    role_member TEXT NOT NULL DEFAULT '',
    ops_channel TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS discord_guilds_owner ON discord_guilds (owner_kind, owner_id);

-- The ops of a corporation are its own. They are posted in the server
-- of its alliance only once a director of the corporation has said so,
-- which is a row here.

CREATE TABLE IF NOT EXISTS discord_ops_shares (
    corporation_id BIGINT PRIMARY KEY,
    set_by         BIGINT NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now()
);

-- What the bot has given is now recorded per server only, in
-- discord_role_grants; the copy that 016 kept on the link goes.
-- checked_at is when a grant was last compared with Discord, and
-- is_member whether the account was in the server then: someone who
-- is not is looked for again only now and then, not on every pass.

ALTER TABLE discord_links DROP COLUMN IF EXISTS roles_applied;
ALTER TABLE discord_links DROP COLUMN IF EXISTS roles_synced_at;

ALTER TABLE discord_role_grants ADD COLUMN IF NOT EXISTS is_member BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE discord_role_grants ADD COLUMN IF NOT EXISTS checked_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE discord_role_grants DROP COLUMN IF EXISTS updated_at;
