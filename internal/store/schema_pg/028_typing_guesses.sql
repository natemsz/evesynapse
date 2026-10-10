-- Schema step 028: typing-intent guesses get their own ring.
--
-- While someone types in a search box, the top suggestions are
-- queued ahead of time (worker_pagewants.go). A guess sits below a
-- record somebody has open and above the proactive orbit, so the
-- viewed ring moves up one: 3 is on screen, 2 is a guess, 1 is
-- named by a character's own records, 0 is one hop further out.
--
-- Guesses are tracked per search box in typing_guesses so that
-- each keystroke replaces the previous one: the box's old guess
-- rows are re-queued (or dropped, when they hold nothing yet)
-- and only the latest keystroke's guesses keep the typing ring.
-- hit_at records a guess the pilot opened within minutes of it
-- being noted — the tuning signal for what is worth guessing.
-- Unhit guesses stay small: one box holds only its latest
-- keystroke, so the table grows with boxes, not keystrokes.
--
-- market_history_wants and type_details had no ring: every want
-- was equal. They gain the same priority, backfilled as viewed,
-- which is what every existing row was noted as.

UPDATE pilot_records SET priority = priority + 1 WHERE priority >= 2;
UPDATE corporation_records SET priority = priority + 1 WHERE priority >= 2;
UPDATE alliance_records SET priority = priority + 1 WHERE priority >= 2;

ALTER TABLE market_history_wants ADD COLUMN IF NOT EXISTS priority BIGINT NOT NULL DEFAULT 3;
ALTER TABLE type_details ADD COLUMN IF NOT EXISTS priority BIGINT NOT NULL DEFAULT 3;
ALTER TABLE type_details ADD COLUMN IF NOT EXISTS noted_at timestamptz;

CREATE TABLE IF NOT EXISTS typing_guesses (
    box        TEXT NOT NULL,
    kind       TEXT NOT NULL,
    entity_id  BIGINT NOT NULL,
    region_id  BIGINT NOT NULL DEFAULT 0,
    noted_at   TIMESTAMPTZ NOT NULL,
    hit_at     TIMESTAMPTZ,
    PRIMARY KEY (box, kind, entity_id, region_id)
);
CREATE INDEX IF NOT EXISTS typing_guesses_open_idx ON typing_guesses (kind, entity_id, region_id) WHERE hit_at IS NULL;
