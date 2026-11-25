-- Schema step 184: the region sweep's last-verified stamp.
--
-- market_region_stats.updated_at says when a type's orders last
-- changed: the sweep rewrites only the types whose stats moved, so
-- an unchanged type keeps an old updated_at however often the sweep
-- verifies its book. Readers that ask "is our own row fresh" (the
-- Fuzzwork swap in market_fallback.go, the appraisal order) read
-- that stamp and mistook verified-but-unchanged rows for stale
-- ones, handing slow-moving types to the fallback.
--
-- swept_at says when a completed sweep last fetched the type's
-- orders, changed or not. The sweep stamps it for every type it
-- fetched in the region; updated_at keeps its last-changed meaning
-- and still shows beside the row as its age. Existing rows start
-- from their last-changed stamp, so nothing reads fresher than it
-- did before this step, and the next completed sweep stamps them.

ALTER TABLE market_region_stats
    ADD COLUMN IF NOT EXISTS swept_at TIMESTAMPTZ;

UPDATE market_region_stats SET swept_at = updated_at WHERE swept_at IS NULL;

ALTER TABLE market_region_stats
    ALTER COLUMN swept_at SET DEFAULT 'epoch'::timestamptz,
    ALTER COLUMN swept_at SET NOT NULL;
