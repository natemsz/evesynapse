-- Schema step 016: Discord accounts linked to EveSynapse accounts.
--
-- One row per EveSynapse account that has connected a Discord
-- account. Only the Discord account's id and name are kept: the
-- sign-in that proves whose it is asks Discord for nothing more, and
-- its token is thrown away once the id has been read.
--
-- A Discord account is linked to one EveSynapse account at a time
-- (discord_id is unique): it belongs to whoever connected it last.
--
-- dm_notifications: the account wants its notifications sent to it on
-- Discord as direct messages. roles_applied is the set of Discord
-- roles EveSynapse last gave the account (sorted ids, comma
-- separated) and roles_synced_at when; the worker compares against
-- them so that it only talks to Discord when something has changed.

CREATE TABLE IF NOT EXISTS discord_links (
    user_id          BIGINT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    discord_id       TEXT NOT NULL UNIQUE,
    username         TEXT NOT NULL,
    linked_at        timestamptz NOT NULL DEFAULT now(),
    dm_notifications BOOLEAN NOT NULL DEFAULT FALSE,
    roles_applied    TEXT NOT NULL DEFAULT '',
    roles_synced_at  timestamptz
);

-- An op is posted to its corporation's Discord channel once;
-- discord_announced_at records that it was (or that it was looked at
-- and there was nowhere to post it).
ALTER TABLE ops ADD COLUMN IF NOT EXISTS discord_announced_at timestamptz;
