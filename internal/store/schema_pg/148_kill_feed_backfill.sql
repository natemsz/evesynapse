-- The killboard list backfill's place in the kills table: kills
-- stored before the feed existed gain their feed rows by a walk in
-- (killmail_time, killmail_id) order from this cursor, a bounded
-- batch per pass. done_at is set when the walk reaches the end of
-- the table; from then on new kills get their feed rows when they
-- are priced, and the walk never runs again.
CREATE TABLE IF NOT EXISTS kill_feed_backfill_state (
    id          INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    cursor_time TIMESTAMPTZ NOT NULL DEFAULT '1970-01-01 00:00:00+00',
    cursor_id   BIGINT NOT NULL DEFAULT 0,
    done_at     TIMESTAMPTZ
);
INSERT INTO kill_feed_backfill_state (id) VALUES (1) ON CONFLICT DO NOTHING;
