-- Schema step 046: a second stored trade-ideas answer, for the
-- Trade ideas page.
--
-- The home module keeps its short answer in row 1. The page asks
-- the model its own, longer question (more ideas per strategy, each
-- with its figures) and keeps that answer in row 2, refreshed beside
-- row 1 on the same cadence and backoff.

ALTER TABLE trade_ideas DROP CONSTRAINT IF EXISTS trade_ideas_id_check;
ALTER TABLE trade_ideas ADD CONSTRAINT trade_ideas_id_check CHECK (id IN (1, 2));
