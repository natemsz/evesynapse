-- Schema step 009: real timestamps, part 1 of 3 (accounts, characters
-- and the snapshot stores).
--
-- Every time in the schema was TEXT: mostly RFC 3339 strings written
-- by the app, a few 'YYYY-MM-DD HH24:MI:SS' strings written by column
-- defaults, and '' standing for "never". The database could not tell
-- a time from any other string, comparisons were string comparisons,
-- and two formats sat in the same tables.
--
-- These columns become timestamptz. What was '' becomes NULL (the
-- column is made nullable where it was NOT NULL DEFAULT ''). A column
-- that is always written keeps NOT NULL. Existing values are
-- converted where they read as a time. Anything else (the app never
-- wrote anything else) becomes NULL, or the epoch in a NOT NULL
-- column, where "as old as it gets" is the safe reading of a stamp.
--
-- Values without a zone were written in UTC, so the conversion reads
-- them as UTC. Each column is cast through text, which makes the
-- step harmless to run again on a database that already has it.
--
-- Changing a column's type rewrites its table, so each table gets one
-- statement covering all of its columns and is rewritten once. The
-- step runs in one transaction: interrupted, it leaves the old
-- columns exactly as they were.

SET LOCAL TIME ZONE 'UTC';

ALTER TABLE users
    ALTER COLUMN created_at DROP DEFAULT,
    ALTER COLUMN created_at TYPE timestamptz USING CASE WHEN created_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN created_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN created_at SET DEFAULT now(),
    ALTER COLUMN last_briefing_at DROP DEFAULT,
    ALTER COLUMN last_briefing_at DROP NOT NULL,
    ALTER COLUMN last_briefing_at TYPE timestamptz USING CASE WHEN last_briefing_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN last_briefing_at::text::timestamptz END;

ALTER TABLE characters
    ALTER COLUMN token_expiry DROP DEFAULT,
    ALTER COLUMN token_expiry DROP NOT NULL,
    ALTER COLUMN token_expiry TYPE timestamptz USING CASE WHEN token_expiry::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN token_expiry::text::timestamptz END,
    ALTER COLUMN cached_until DROP DEFAULT,
    ALTER COLUMN cached_until DROP NOT NULL,
    ALTER COLUMN cached_until TYPE timestamptz USING CASE WHEN cached_until::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN cached_until::text::timestamptz END,
    ALTER COLUMN created_at DROP DEFAULT,
    ALTER COLUMN created_at TYPE timestamptz USING CASE WHEN created_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN created_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN created_at SET DEFAULT now(),
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN updated_at SET DEFAULT now(),
    ALTER COLUMN link_state_at DROP DEFAULT,
    ALTER COLUMN link_state_at DROP NOT NULL,
    ALTER COLUMN link_state_at TYPE timestamptz USING CASE WHEN link_state_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN link_state_at::text::timestamptz END;

ALTER TABLE character_snapshots
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN cached_until DROP DEFAULT,
    ALTER COLUMN cached_until DROP NOT NULL,
    ALTER COLUMN cached_until TYPE timestamptz USING CASE WHEN cached_until::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN cached_until::text::timestamptz END;

ALTER TABLE global_snapshots
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN cached_until DROP DEFAULT,
    ALTER COLUMN cached_until TYPE timestamptz USING CASE WHEN cached_until::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN cached_until::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE killmail_details
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE contract_details
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE war_details
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE snapshot_fetch_state
    ALTER COLUMN attempted_at DROP DEFAULT,
    ALTER COLUMN attempted_at TYPE timestamptz USING CASE WHEN attempted_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN attempted_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE character_corporations
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE skill_plans
    ALTER COLUMN created_at DROP DEFAULT,
    ALTER COLUMN created_at TYPE timestamptz USING CASE WHEN created_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN created_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE widget_configs
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE wallet_history
    ALTER COLUMN sampled_at DROP DEFAULT,
    ALTER COLUMN sampled_at TYPE timestamptz USING CASE WHEN sampled_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN sampled_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE local_fittings
    ALTER COLUMN created_at DROP DEFAULT,
    ALTER COLUMN created_at TYPE timestamptz USING CASE WHEN created_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN created_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN created_at SET DEFAULT now(),
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN updated_at SET DEFAULT now();
