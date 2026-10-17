-- Schema step 051: true values -- the bottom-up price of a
-- manufacturable type, for the types the market cannot price.
--
-- CCP's adjusted price is the only number /markets/prices/
-- publishes for contract-traded ships (supercapitals trade by
-- contract, never on the open market), and for them it is a
-- fossil: an Avatar's adjusted price sits near 65B while the
-- ship trades around 600B. The worker therefore prices every
-- manufacturable type from its bill of materials (recursive
-- decomposition at a standard facility profile, inputs at The
-- Forge's typical sell) and, for types with no real market,
-- estimates a sell price from public contracts clustered
-- around that build cost.
--
-- One row per type the worker has priced:
--   build_cost   per-unit build cost at the standard profile
--   est_sell     estimated sell price; NULL while the type has
--                a real market (the market price rules there
--                and consumers never reach this column)
--   sell_samples public-contract samples behind est_sell;
--                0 means est_sell is build_cost itself, the
--                anchored fallback
--   computed_at  when the row was computed
--   profile_version  the pricing profile that produced it;
--                a profile change bumps the version and every
--                row goes stale at once
--
-- Consumers resolve a unit price as: market average, then
-- est_sell, then CCP's adjusted price (see true_values.go).

CREATE TABLE IF NOT EXISTS true_values (
    type_id BIGINT PRIMARY KEY,
    build_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
    est_sell DOUBLE PRECISION,
    sell_samples BIGINT NOT NULL DEFAULT 0,
    computed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    profile_version BIGINT NOT NULL DEFAULT 0
);
