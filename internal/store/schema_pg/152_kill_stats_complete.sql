-- The stats rebuild runs twice. The first pass runs at once and
-- counts kills, losses, ISK and systems from the permanent
-- involvement record (done_at). The second runs once the attacker
-- restore has drained, so ships flown count from complete rows
-- (complete_at). The retention prune waits for complete_at.
ALTER TABLE kill_stats_rebuild_state ADD COLUMN IF NOT EXISTS complete_at TIMESTAMPTZ;
