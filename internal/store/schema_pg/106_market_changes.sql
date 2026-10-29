-- Schema step 106: the market change feed.
--
-- When a region's sweep completes, the same transaction compares each
-- stat pack row with its last stored day and records what changed
-- enough to tell someone:
--
--   price_up / price_down  the typical sell moved by at least the
--                          floor and by three times the type's own
--                          daily volatility (a jumpy item has to jump
--                          further to count)
--   supply_down            the volume on sale fell by half or more
--   spread_wide            the gap between typical sell and buy at
--                          least doubled, to 10% or more
--
-- Only books with some depth and recorded trade qualify. One row per
-- (region, type, day, kind); a later sweep the same day updates the
-- figures and keeps when it was first seen. Rows older than 30 days
-- are pruned by the same pass. The Trends page, the item page and
-- the notification pass read these rows; none of them recompute.

CREATE TABLE IF NOT EXISTS market_changes (
    region_id   BIGINT NOT NULL,
    type_id     BIGINT NOT NULL,
    day         TEXT NOT NULL,
    kind        TEXT NOT NULL,
    before      DOUBLE PRECISION NOT NULL,
    after       DOUBLE PRECISION NOT NULL,
    pct         DOUBLE PRECISION NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (region_id, type_id, day, kind)
);
CREATE INDEX IF NOT EXISTS market_changes_region_day_idx ON market_changes (region_id, day DESC);
CREATE INDEX IF NOT EXISTS market_changes_type_idx ON market_changes (type_id, day DESC);
