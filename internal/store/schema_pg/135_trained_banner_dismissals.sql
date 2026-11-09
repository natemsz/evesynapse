-- Schema step 135: trained banner dismissals.
--
-- The character sheet's "trained since your last visit" banner
-- reads the notification store on every render, including skill
-- events read within a short window (the click-through that
-- brought the visitor just marked them). Its dismiss hides only
-- the entries on screen, not every alert: one row per account,
-- pilot and dismissed notification, keyed by the notification's
-- own title, so later finishes still announce. Keys whose
-- notifications are no longer recent are pruned at render, so
-- the table only ever holds live hides.
CREATE TABLE IF NOT EXISTS trained_banner_dismissals (
    user_id      BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    character_id BIGINT NOT NULL,
    entry_key    TEXT NOT NULL,
    dismissed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, character_id, entry_key)
);
