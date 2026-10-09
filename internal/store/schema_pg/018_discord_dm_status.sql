-- Schema step 018: why a Discord direct message did not arrive.
--
-- A notification sent to someone as a direct message can be refused by
-- Discord (the person does not take messages from members of the
-- server, or shares no server with the bot). That used to be written
-- only to the log, where the person it concerns never sees it.
-- dm_problem holds what went wrong with the last message, in words for
-- the settings page, and dm_problem_at when; both are cleared by the
-- next message that goes through.

ALTER TABLE discord_links ADD COLUMN IF NOT EXISTS dm_problem TEXT NOT NULL DEFAULT '';
ALTER TABLE discord_links ADD COLUMN IF NOT EXISTS dm_problem_at timestamptz;
