-- The attacker restore reads the daily killmail archives
-- (killboard_archive.go), newest day first. day is the last day
-- read; done_at marks the walk reaching the first archive day.
-- Kills past retention get their ships flown counted into the
-- daily aggregates without keeping their attacker rows, so the
-- rebuild's second pass (schema 152) is gone.
CREATE TABLE IF NOT EXISTS kill_archive_restore_state (
    id      INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    day     DATE,
    done_at TIMESTAMPTZ
);
INSERT INTO kill_archive_restore_state (id) VALUES (1) ON CONFLICT DO NOTHING;
ALTER TABLE kill_stats_rebuild_state DROP COLUMN IF EXISTS complete_at;
