-- Schema step 011: real timestamps, part 3 of 3 (the name and record
-- caches).
--
-- The same conversion as steps 009 and 010, for what the app looks up
-- and keeps: structure and planet names, pilot, corporation and
-- alliance records, pilot-name lookups and item descriptions.
--
-- Most of these rows exist before they are filled in: a name waiting
-- to be resolved, a record waiting to be fetched. Their time was ''
-- until then, so those columns become nullable and NULL means "not
-- yet". After this step no time in the schema is kept as text.

SET LOCAL TIME ZONE 'UTC';

ALTER TABLE structure_names
    ALTER COLUMN resolved_at DROP DEFAULT,
    ALTER COLUMN resolved_at DROP NOT NULL,
    ALTER COLUMN resolved_at TYPE timestamptz USING CASE WHEN resolved_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN resolved_at::text::timestamptz END;

ALTER TABLE structure_context
    ALTER COLUMN updated_at DROP DEFAULT,
    ALTER COLUMN updated_at TYPE timestamptz USING CASE WHEN updated_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN updated_at::text::timestamptz ELSE 'epoch'::timestamptz END;

ALTER TABLE planet_names
    ALTER COLUMN resolved_at DROP DEFAULT,
    ALTER COLUMN resolved_at DROP NOT NULL,
    ALTER COLUMN resolved_at TYPE timestamptz USING CASE WHEN resolved_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN resolved_at::text::timestamptz END;

ALTER TABLE pilot_records
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at DROP NOT NULL,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz END;

ALTER TABLE corporation_records
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at DROP NOT NULL,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz END;

ALTER TABLE alliance_records
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at DROP NOT NULL,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz END;

ALTER TABLE pilot_name_wants
    ALTER COLUMN requested_at DROP DEFAULT,
    ALTER COLUMN requested_at TYPE timestamptz USING CASE WHEN requested_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN requested_at::text::timestamptz ELSE 'epoch'::timestamptz END,
    ALTER COLUMN resolved_at DROP DEFAULT,
    ALTER COLUMN resolved_at DROP NOT NULL,
    ALTER COLUMN resolved_at TYPE timestamptz USING CASE WHEN resolved_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN resolved_at::text::timestamptz END,
    ALTER COLUMN next_try_at DROP DEFAULT,
    ALTER COLUMN next_try_at DROP NOT NULL,
    ALTER COLUMN next_try_at TYPE timestamptz USING CASE WHEN next_try_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN next_try_at::text::timestamptz END;

ALTER TABLE type_details
    ALTER COLUMN fetched_at DROP DEFAULT,
    ALTER COLUMN fetched_at DROP NOT NULL,
    ALTER COLUMN fetched_at TYPE timestamptz USING CASE WHEN fetched_at::text ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}[T ][0-9]{2}:[0-9]{2}:[0-9]{2}' THEN fetched_at::text::timestamptz END;
