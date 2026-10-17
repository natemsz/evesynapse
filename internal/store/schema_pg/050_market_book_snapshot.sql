-- Schema step 050: the stored market book, and site settings.
--
-- A completed region sweep used to distill its staged book into
-- per-type stats and then throw the book itself away, so the
-- Market page's order book could only be read live -- and
-- anonymous visitors, who never fetch, were locked out of it.
-- Now the sweep also keeps the top orders per side (the item
-- page shows five) in market_book_orders, replaced wholesale
-- inside the sweep completion transaction like the stats it
-- sits beside. The staging table gains the order's system id,
-- which the book's location column renders from.
--
-- site_settings is the operator's live settings: one row per
-- key, flipped from the Admin page without a restart. Its first
-- key is market_anon (the anonymous-market switch); the
-- MARKET_ANON environment variable is only the boot default
-- when no row exists yet.

ALTER TABLE market_sweep_orders ADD COLUMN IF NOT EXISTS system_id BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS market_book_orders (
    region_id BIGINT NOT NULL,
    type_id BIGINT NOT NULL,
    is_buy_order BIGINT NOT NULL DEFAULT 0,
    rank BIGINT NOT NULL DEFAULT 0,
    price DOUBLE PRECISION NOT NULL DEFAULT 0,
    volume_remain BIGINT NOT NULL DEFAULT 0,
    location_id BIGINT NOT NULL DEFAULT 0,
    system_id BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (region_id, type_id, is_buy_order, rank)
);

CREATE TABLE IF NOT EXISTS site_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
