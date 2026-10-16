-- Schema step 049: trade_ideas.rows, the scanner rows each answer
-- was written from.
--
-- The model used to copy an idea's figures out of the CSV it was
-- given, and the stored answer carried its own transcription. Now
-- the answer references its rows ("(#n)") and the page composes the
-- figure segment from the stored row set: the CSV rows as sent
-- (number, item, station, buy, sell, spread, sold/day, daily
-- profit — the pre-formatted strings), as one JSON array in a
-- nullable TEXT column. Answers written before this step have no
-- row set and render verbatim, exactly as they do today. Additive
-- only: a rollback binary ignores the column, like step 048.

ALTER TABLE trade_ideas ADD COLUMN IF NOT EXISTS rows TEXT;
