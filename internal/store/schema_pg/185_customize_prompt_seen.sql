-- Schema step 185: the Customize hint's one-time stamp.
--
-- The home shows a small card under the Customize button once,
-- telling the account the page can be arranged and that new
-- modules have joined the list. customize_prompt_seen_at records
-- that the account has answered it (opened Customize, followed
-- the card in, or said not now), so it is never shown unasked
-- again. NULL means it has not been answered yet.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS customize_prompt_seen_at TIMESTAMPTZ;
