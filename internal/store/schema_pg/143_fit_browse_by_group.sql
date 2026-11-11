-- Schema step 143: the Browse tree's All view reads one market
-- group's types across every family, so the types table needs an
-- index led by the group.
CREATE INDEX IF NOT EXISTS fit_browse_types_by_group_idx ON fit_browse_types (market_group_id, name);
