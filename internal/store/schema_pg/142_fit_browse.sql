-- Schema step 142: the workbench's Browse tree, built at SDE import.
--
-- For each slot family (high, medium, low, rig, subsystem, drone,
-- fighter, charge) the market groups that hold its types, with
-- their parents, and the types themselves, so the tree reads one
-- level at a time by key.
CREATE TABLE IF NOT EXISTS fit_browse_groups (
    family          TEXT   NOT NULL,
    market_group_id BIGINT NOT NULL,
    parent_group_id BIGINT NOT NULL DEFAULT 0,
    name            TEXT   NOT NULL,
    type_count      INT    NOT NULL DEFAULT 0,
    PRIMARY KEY (family, market_group_id)
);
CREATE INDEX IF NOT EXISTS fit_browse_groups_parent_idx ON fit_browse_groups (family, parent_group_id, name);

CREATE TABLE IF NOT EXISTS fit_browse_types (
    family          TEXT   NOT NULL,
    type_id         BIGINT NOT NULL,
    market_group_id BIGINT NOT NULL,
    name            TEXT   NOT NULL,
    PRIMARY KEY (family, type_id)
);
CREATE INDEX IF NOT EXISTS fit_browse_types_group_idx ON fit_browse_types (family, market_group_id, name);
