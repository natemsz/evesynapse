-- Schema step 021: removing people who do not belong from a server.
--
-- Off unless the directors of a server switch it on. When on, the bot
-- removes from the server anyone who is owed no role by the server's
-- rules: someone whose character left, whose account or Discord
-- connection went, or who never connected EveSynapse at all.
--
--   kick_enabled       the switch
--   kick_exempt_roles  roles whose holders are never removed (guests,
--                      diplomats), sorted ids, comma separated
--   kick_checked_at    when the server's member list was last gone
--                      through, so that it is not read every pass

ALTER TABLE discord_guilds ADD COLUMN IF NOT EXISTS kick_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE discord_guilds ADD COLUMN IF NOT EXISTS kick_exempt_roles TEXT NOT NULL DEFAULT '';
ALTER TABLE discord_guilds ADD COLUMN IF NOT EXISTS kick_checked_at timestamptz;
