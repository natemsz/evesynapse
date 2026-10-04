-- ---------------------------------------------------------------------
-- Market browse tree (schema 024, v0.3.10): the invMarketGroups
-- hierarchy behind the Market page's category browser. This is the
-- market taxonomy (where a player finds an item for sale), separate
-- from sde_groups/sde_categories (what kind of thing an item is).
-- parent_group_id is 0 for top-level groups; the dump's empty/root
-- parent values normalize to 0 at import time.
-- ---------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS sde_market_groups (
    market_group_id INTEGER PRIMARY KEY, -- EVE market group ID
    parent_group_id INTEGER NOT NULL DEFAULT 0, -- 0 = top level
    name            TEXT    NOT NULL,
    icon_id         INTEGER NOT NULL DEFAULT 0, -- dump icon ID, kept for reference
    has_types       INTEGER NOT NULL DEFAULT 0 -- 1 when the group directly lists types
);

CREATE INDEX IF NOT EXISTS idx_sde_market_groups_parent
    ON sde_market_groups (parent_group_id, name);

CREATE INDEX IF NOT EXISTS idx_sde_market_groups_name
    ON sde_market_groups (name);
