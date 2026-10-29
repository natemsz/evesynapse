-- Briefing dismissals (schema 101, home_briefing.go): the briefing
-- lines an account has hidden. A dismissal is not a deletion — the
-- line is recomputed on every home render, and while it still
-- applies it renders in the briefing's dismissed list (behind the
-- modal) instead of the stacks. Rows whose line no longer applies
-- are pruned at render, so the table only ever holds live hides.
CREATE TABLE IF NOT EXISTS briefing_dismissals (
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    line_key     TEXT NOT NULL,
    dismissed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, line_key)
);
