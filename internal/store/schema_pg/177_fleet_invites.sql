-- Step 177: fleet invites sent from EveSynapse. A pilot signed up for a
-- running fleet can say they are ready for an invite (ready_at), and
-- those running it queue invites for one pilot or many (op_invites).
-- The fleet worker sends them through ESI as the fleet's boss, which
-- has to have granted EveSynapse fleet invites, and records how each
-- went.
ALTER TABLE op_signups ADD COLUMN IF NOT EXISTS ready_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS op_invites (
    op_id        BIGINT NOT NULL REFERENCES ops(id) ON DELETE CASCADE,
    character_id BIGINT NOT NULL,
    queued_by    BIGINT NOT NULL,
    queued_at    TIMESTAMPTZ NOT NULL,
    sent_at      TIMESTAMPTZ,
    failed_at    TIMESTAMPTZ,
    error        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (op_id, character_id)
);
CREATE INDEX IF NOT EXISTS op_invites_pending ON op_invites (queued_at)
    WHERE sent_at IS NULL AND failed_at IS NULL;
