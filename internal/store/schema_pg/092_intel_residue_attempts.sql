-- The residue step's attempt accounting (schema 092,
-- internal/app/residue_model.go): one row per report a pass has
-- been tried on, so a failing pass is durable instead of a
-- timeout the backlog sweep re-offers forever. attempts counts
-- failed passes over the report's current text (text_hash); a
-- changed text starts the count over. next_attempt_at is the
-- backoff gate the sweep honours; dead_reason, once set, takes
-- the report out of the backlog for good, with the reason it
-- left recorded. intel_residue_calls gains the text hash of the
-- report a completed pass read, so identical report text is
-- never read twice.
CREATE TABLE IF NOT EXISTS intel_residue_attempts (
    report_id       BIGINT PRIMARY KEY,
    text_hash       TEXT NOT NULL DEFAULT '',
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_attempt_at TIMESTAMPTZ,
    next_attempt_at TIMESTAMPTZ,
    dead_reason     TEXT NOT NULL DEFAULT ''
);

ALTER TABLE intel_residue_calls ADD COLUMN IF NOT EXISTS text_hash TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS intel_residue_calls_hash_idx ON intel_residue_calls (text_hash);
