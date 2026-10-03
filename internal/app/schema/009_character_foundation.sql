-- EveSynapse multi-character foundation (schema addition 009).
-- Phase 1A: everything the app needs to run an account with dozens
-- of linked characters safely.
--
-- owner_hash is the EVE SSO `owner` claim (the character owner
-- hash) from the last verified login. It changes when a character
-- changes EVE accounts (sold/transferred); a later login carrying
-- a different hash flags the link instead of trusting it quietly
-- (link_state = 'owner_changed', see characters.link_state).
--
-- link_state is the worker-facing health of the link:
--   'ok'            — sync normally;
--   'token_dead'    — CCP definitively rejected the refresh token
--                     (revoked/expired): stop syncing until the
--                     user signs the character in again;
--   'owner_changed' — the owner hash changed since the link was
--                     verified: stop syncing until a fresh sign-in
--                     re-verifies control.
-- link_state_at records when the state was last set (RFC3339).
--
-- tags is the user's own free-text grouping for the character
-- ("PI alt, Trader"): ESI cannot reveal which characters share a
-- game account, so fleets are organized by tags instead.

ALTER TABLE characters ADD COLUMN owner_hash TEXT NOT NULL DEFAULT '';

ALTER TABLE characters ADD COLUMN tags TEXT NOT NULL DEFAULT '';

ALTER TABLE characters ADD COLUMN link_state TEXT NOT NULL DEFAULT 'ok';

ALTER TABLE characters ADD COLUMN link_state_at TEXT;
