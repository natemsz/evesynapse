-- Schema step 118: per-location asset values for the Assets page.
--
-- One row per character per place their assets sit in, folded from
-- the character's stored asset snapshot at ingest time: the stack
-- and item counts, the ship count, the top-level volume, and the
-- estimated value of everything filed under the place (fitted
-- modules and cargo included) at the house price order (the swept
-- typical sell, then the stored estimated sell). priced_stacks
-- counts the stacks a house price could value, so the page can say
-- how much of a place is priced instead of quietly valuing the
-- rest at zero.

CREATE TABLE IF NOT EXISTS asset_location_values (
    user_id       BIGINT NOT NULL,
    character_id  BIGINT NOT NULL,
    location_id   BIGINT NOT NULL,
    location_type TEXT NOT NULL DEFAULT '',
    est_value     DOUBLE PRECISION NOT NULL DEFAULT 0,
    priced_stacks BIGINT NOT NULL DEFAULT 0,
    total_stacks  BIGINT NOT NULL DEFAULT 0,
    total_items   BIGINT NOT NULL DEFAULT 0,
    ships         BIGINT NOT NULL DEFAULT 0,
    volume        DOUBLE PRECISION NOT NULL DEFAULT 0,
    computed_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, character_id, location_id)
);
CREATE INDEX IF NOT EXISTS asset_location_values_user_idx ON asset_location_values (user_id, location_id);
