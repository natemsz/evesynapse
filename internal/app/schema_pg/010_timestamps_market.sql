-- Schema step 010: real timestamps, part 2 of 3 (the market).
--
-- The same conversion as step 009, for the market tables: every time
-- kept as TEXT becomes a timestamptz, read as UTC. See that step for
-- how existing values are carried over.
--
-- One column here could say "never": an order's closed_at was '' while
-- the order was still open. It becomes nullable, and NULL means open.
-- Every other column is written with each row and stays NOT NULL.

SET LOCAL TIME ZONE 'UTC';

ALTER TABLE guide_prices_meta
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN cached_until DROP DEFAULT,
    ALTER COLUMN cached_until TYPE timestamptz USING CASE WHEN cached_until::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN cached_until::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE guide_price_wants
    ALTER COLUMN wanted_at DROP DEFAULT,
    ALTER COLUMN wanted_at TYPE timestamptz USING CASE WHEN wanted_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN wanted_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_history_wants
    ALTER COLUMN last_requested_at DROP DEFAULT,
    ALTER COLUMN last_requested_at TYPE timestamptz USING CASE WHEN last_requested_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN last_requested_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_fetch_state
    ALTER COLUMN attempted_at DROP DEFAULT,
    ALTER COLUMN attempted_at TYPE timestamptz USING CASE WHEN attempted_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN attempted_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_watchlist
    ALTER COLUMN created_at DROP DEFAULT,
    ALTER COLUMN created_at TYPE timestamptz USING CASE WHEN created_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN created_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE order_health
    ALTER COLUMN computed_at DROP DEFAULT,
    ALTER COLUMN computed_at TYPE timestamptz USING CASE WHEN computed_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN computed_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE order_lifecycle
    ALTER COLUMN first_seen_at DROP DEFAULT,
    ALTER COLUMN first_seen_at TYPE timestamptz USING CASE WHEN first_seen_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN first_seen_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN last_seen_at DROP DEFAULT,
    ALTER COLUMN last_seen_at TYPE timestamptz USING CASE WHEN last_seen_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN last_seen_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN closed_at DROP DEFAULT,
    ALTER COLUMN closed_at DROP NOT NULL,
    ALTER COLUMN closed_at TYPE timestamptz USING CASE WHEN closed_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN closed_at::text::timestamptz END;

ALTER TABLE market_region_stats
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_station_stats
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_sweep_state
    ALTER COLUMN started_at DROP DEFAULT,
    ALTER COLUMN started_at TYPE timestamptz USING CASE WHEN started_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN started_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE market_station_leaderboard
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;
