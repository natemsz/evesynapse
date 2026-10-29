-- Schema step 110: watchlist baskets.
--
-- A player can watch a whole basket next to single items: the row
-- names the basket (their own, or a preset) and the alert line for
-- its stored daily total. Values come from
-- market_basket_values_daily; this table only records who watches
-- what, at what percent. Deleting the basket or the account clears
-- the watch with it.

CREATE TABLE IF NOT EXISTS market_watchlist_baskets (
    user_id       BIGINT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    basket_id     BIGINT NOT NULL REFERENCES market_baskets (id) ON DELETE CASCADE,
    threshold_pct DOUBLE PRECISION NOT NULL DEFAULT 5,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, basket_id)
);
