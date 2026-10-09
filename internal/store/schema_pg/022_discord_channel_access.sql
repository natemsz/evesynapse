-- Schema step 022: which roles can open which channels.
--
-- A server's directors can have the bot keep a channel private to
-- chosen roles: the channel is hidden from everyone else, and each
-- role listed for it here can see it. One row is one role allowed in
-- one channel. A channel with no rows is not managed: the bot leaves
-- its permissions exactly as it finds them.

CREATE TABLE IF NOT EXISTS discord_channel_access (
    guild_id   TEXT NOT NULL REFERENCES discord_guilds (guild_id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    role_id    TEXT NOT NULL,
    set_by     BIGINT NOT NULL DEFAULT 0,
    set_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (guild_id, channel_id, role_id)
);
