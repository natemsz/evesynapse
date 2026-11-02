-- Schema step 118: market station heat: how often the best price
-- at a station was beaten, per item and side.
--
-- market_best_beats is the sweep worker's log: one row each time a
-- completed sweep finds a station's best sell lower than the best
-- the previous sweep stored, or its best buy higher. Rows are
-- pruned once they are a day old. market_station_heat is the
-- trailing 12 hour count per (station, item, side), refreshed in
-- the same sweep transaction, so pages read the finished count
-- instead of counting events at render time. A pair with no beats
-- in the window has no heat row and reads as quiet.

CREATE TABLE IF NOT EXISTS market_best_beats (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    location_id BIGINT NOT NULL,
    region_id BIGINT NOT NULL,
    type_id BIGINT NOT NULL,
    is_buy_order BIGINT NOT NULL DEFAULT 0,
    at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS market_best_beats_pair_idx ON market_best_beats (location_id, type_id, is_buy_order, at);

CREATE TABLE IF NOT EXISTS market_station_heat (
    location_id BIGINT NOT NULL,
    region_id BIGINT NOT NULL,
    type_id BIGINT NOT NULL,
    is_buy_order BIGINT NOT NULL DEFAULT 0,
    beats_12h BIGINT NOT NULL DEFAULT 0,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (location_id, type_id, is_buy_order)
);
