-- Schema step 020: adding people to a server automatically.
--
-- With the member's say-so (Discord's "join servers for you", asked
-- for when they connect), the bot can put a connected account into
-- the server of a corporation or alliance it belongs to, instead of
-- waiting for them to find an invite. Doing that later, when a
-- character joins a corporation or a server is set up, needs the
-- account's Discord token, so it is kept: sealed with the install's
-- token key, exactly as EVE tokens are. It can add that account to
-- servers the bot is in and read its name; nothing else was asked for.
--
-- auto_join: the directors of a server may switch the adding off.

ALTER TABLE discord_links ADD COLUMN IF NOT EXISTS access_token TEXT NOT NULL DEFAULT '';
ALTER TABLE discord_links ADD COLUMN IF NOT EXISTS refresh_token TEXT NOT NULL DEFAULT '';
ALTER TABLE discord_links ADD COLUMN IF NOT EXISTS token_expiry timestamptz;

ALTER TABLE discord_guilds ADD COLUMN IF NOT EXISTS auto_join BOOLEAN NOT NULL DEFAULT TRUE;

-- wanted: the roles the account was owed in the server when it was
-- last looked for there. Someone who was not in the server is looked
-- for again as soon as that changes (a character has joined the
-- corporation, say), instead of at the next periodic check.
ALTER TABLE discord_role_grants ADD COLUMN IF NOT EXISTS wanted TEXT NOT NULL DEFAULT '';
