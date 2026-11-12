-- Schema step 151: the PI colony planner.
--
-- A celestial's type and radius: the planner places colony
-- structures on a planet's surface, so link lengths (and with them
-- link CPU and power) follow from the radius, and the planet's type
-- decides what it can extract. Both come from the mapDenormalize rows
-- the celestials already read (radius in metres). Rows fill in with
-- the next SDE import (sde_import_version 22 forces one).
ALTER TABLE sde_celestials ADD COLUMN IF NOT EXISTS type_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE sde_celestials ADD COLUMN IF NOT EXISTS radius DOUBLE PRECISION NOT NULL DEFAULT 0;

-- Solved colony plans. Solving runs once, when the player asks; the
-- page reads the stored answer. The token is the plan's address, so a
-- plan can be shared by link. Plans made without an account are
-- cleared after 30 days; an account keeps its recent ones.
CREATE TABLE IF NOT EXISTS pi_plans (
    token      TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    request    JSONB NOT NULL,
    solution   JSONB NOT NULL
);
CREATE INDEX IF NOT EXISTS pi_plans_user_idx ON pi_plans (user_id, created_at DESC);
