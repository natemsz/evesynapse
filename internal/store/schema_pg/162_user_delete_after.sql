-- Schema step 162: accounts emptied by a merge.
--
-- delete_after: when an account whose characters and saved data were
-- moved into another account is removed. The worker deletes it once
-- the time has passed, provided it still has no character. NULL for
-- every account not waiting on removal.
ALTER TABLE users ADD COLUMN IF NOT EXISTS delete_after TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS users_delete_after_idx ON users (delete_after) WHERE delete_after IS NOT NULL;
