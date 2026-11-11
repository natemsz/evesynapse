-- Schema step 141: each variation family's compare table, built at
-- SDE import.
--
-- A family is a base type and the types whose base it is. Columns
-- are the attributes the table shows, in order; each member row
-- carries its values in that order (NaN where it has none).
CREATE TABLE IF NOT EXISTS sde_variation_columns (
    root_type_id BIGINT   NOT NULL,
    ord          SMALLINT NOT NULL,
    attribute_id BIGINT   NOT NULL,
    PRIMARY KEY (root_type_id, ord)
);

CREATE TABLE IF NOT EXISTS sde_variations (
    type_id       BIGINT             PRIMARY KEY,
    root_type_id  BIGINT             NOT NULL,
    meta_group_id BIGINT             NOT NULL DEFAULT 0,
    meta_level    DOUBLE PRECISION   NOT NULL DEFAULT 0,
    vals          DOUBLE PRECISION[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS sde_variations_root_idx ON sde_variations (root_type_id);
