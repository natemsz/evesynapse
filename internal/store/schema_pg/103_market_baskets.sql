-- Schema step 103: market baskets.
--
-- A basket is a named list of item types and quantities priced in
-- one hub region: the presets everyone sees (user_id NULL, a stable
-- preset_key) and the baskets a player keeps for themselves. Its
-- value over time is stored per day in market_basket_values_daily,
-- written from the stat pack: today's row in the transaction that
-- completes the region's sweep, and the whole stored history when a
-- basket's contents change (from market_region_stats_daily). Pages
-- read stored values only.

CREATE TABLE IF NOT EXISTS market_baskets (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id    BIGINT REFERENCES users (id) ON DELETE CASCADE,
    preset_key TEXT NOT NULL DEFAULT '',
    name       TEXT NOT NULL,
    region_id  BIGINT NOT NULL DEFAULT 10000002,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS market_baskets_preset_idx
    ON market_baskets (preset_key) WHERE user_id IS NULL;
CREATE INDEX IF NOT EXISTS market_baskets_user_idx ON market_baskets (user_id);
CREATE INDEX IF NOT EXISTS market_baskets_region_idx ON market_baskets (region_id);

CREATE TABLE IF NOT EXISTS market_basket_items (
    basket_id BIGINT NOT NULL REFERENCES market_baskets (id) ON DELETE CASCADE,
    type_id   BIGINT NOT NULL,
    quantity  BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (basket_id, type_id)
);

-- One row per basket and day. value_sell / value_buy sum quantity
-- times the typical sell / buy over the items priced that day;
-- priced_items says how many of item_count had a price, so a
-- partial day is never read as the whole basket.
CREATE TABLE IF NOT EXISTS market_basket_values_daily (
    basket_id    BIGINT NOT NULL REFERENCES market_baskets (id) ON DELETE CASCADE,
    day          TEXT NOT NULL,
    value_sell   DOUBLE PRECISION NOT NULL DEFAULT 0,
    value_buy    DOUBLE PRECISION NOT NULL DEFAULT 0,
    priced_items BIGINT NOT NULL DEFAULT 0,
    item_count   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (basket_id, day)
);

-- The presets, in The Forge. Type ids are the game's own.
INSERT INTO market_baskets (preset_key, name, region_id)
VALUES ('minerals', 'Minerals', 10000002),
       ('ice', 'Ice products', 10000002),
       ('fuel', 'Fuel blocks', 10000002),
       ('moon_r4', 'Moon materials (R4)', 10000002)
ON CONFLICT DO NOTHING;

INSERT INTO market_basket_items (basket_id, type_id, quantity)
SELECT b.id, v.type_id, v.quantity
FROM market_baskets b
JOIN (VALUES
    ('minerals', 34, 1), ('minerals', 35, 1), ('minerals', 36, 1), ('minerals', 37, 1),
    ('minerals', 38, 1), ('minerals', 39, 1), ('minerals', 40, 1), ('minerals', 11399, 1),
    ('ice', 16272, 1), ('ice', 16273, 1), ('ice', 16274, 1), ('ice', 16275, 1),
    ('ice', 17887, 1), ('ice', 17888, 1), ('ice', 17889, 1),
    ('fuel', 4051, 1), ('fuel', 4246, 1), ('fuel', 4247, 1), ('fuel', 4312, 1),
    ('moon_r4', 16633, 1), ('moon_r4', 16634, 1), ('moon_r4', 16635, 1), ('moon_r4', 16636, 1)
) AS v (preset_key, type_id, quantity) ON v.preset_key = b.preset_key
WHERE b.user_id IS NULL
ON CONFLICT DO NOTHING;
