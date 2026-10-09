-- Schema step 023: when each account was last seen.
--
-- How often the worker refreshes an account's characters follows how
-- recently the account was used (internal/app/worker_tiers.go). That
-- was kept in memory only, so every restart forgot it, and it could
-- not tell an account away for an afternoon from one gone for months.
--
-- last_seen_at is the time of the account's latest request, written
-- at most once an hour while it is in use. A new account starts as
-- seen now.
--
-- Nothing is known of the accounts that exist when this step runs, so
-- they are given a time just over a day before it: that is "dormant",
-- which is what every account was after a restart until now, and a
-- week has to pass from there before any of them counts as asleep.

ALTER TABLE users ADD COLUMN IF NOT EXISTS last_seen_at timestamptz NOT NULL DEFAULT (now() - interval '25 hours');
ALTER TABLE users ALTER COLUMN last_seen_at SET DEFAULT now();
