-- Schema step 076: the shared demo account.
--
-- One account, flagged on the user row, holds the seeded demo
-- pilot every visitor can test-drive from the landing page. The
-- flag is what the write gates and the worker's refresh
-- eligibility check key on: the account has no EVE tokens, so it
-- must never be queued for a sync, and its writes are turned
-- away server-side because the account is shared.
--
-- demo_state is the reset clock: a single row whose timestamp
-- says when the seed data was last restored, so the worker's
-- daily pass can put the account back to its seed state.

ALTER TABLE users ADD COLUMN IF NOT EXISTS is_demo BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS demo_state (
    id            BIGINT PRIMARY KEY,
    last_reset_at timestamptz NOT NULL DEFAULT now()
);
