-- Killboard statistics (schema 131): per entity, per day kill and
-- loss aggregates folded at ingest (killboard_stats.go), so the
-- killboard and pilot pages read stored rows only and never
-- aggregate the kills table at render. Values are the kill's
-- priced value (value_ours, else zKillboard's figure) at fold
-- time. The day bucket lets a page sum any window up to all time
-- from a few hundred rows at most.

CREATE TABLE IF NOT EXISTS kill_entity_daily (
    day                 DATE NOT NULL,
    kind                TEXT NOT NULL,
    entity_id           BIGINT NOT NULL,
    kills               BIGINT NOT NULL DEFAULT 0,
    solo                BIGINT NOT NULL DEFAULT 0,
    losses              BIGINT NOT NULL DEFAULT 0,
    isk_killed          DOUBLE PRECISION NOT NULL DEFAULT 0,
    isk_lost            DOUBLE PRECISION NOT NULL DEFAULT 0,
    biggest_killmail_id BIGINT NOT NULL DEFAULT 0,
    biggest_value       DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (day, kind, entity_id)
);

CREATE TABLE IF NOT EXISTS kill_entity_daily_ships (
    day          DATE NOT NULL,
    kind         TEXT NOT NULL,
    entity_id    BIGINT NOT NULL,
    ship_type_id BIGINT NOT NULL,
    flown        BIGINT NOT NULL DEFAULT 0,
    lost         BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, kind, entity_id, ship_type_id)
);

CREATE TABLE IF NOT EXISTS kill_entity_daily_systems (
    day       DATE NOT NULL,
    kind      TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    system_id BIGINT NOT NULL,
    events    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (day, kind, entity_id, system_id)
);

-- One marker per (kill, entity) already folded into the daily
-- aggregates: ingest, the pricing pass and the backfill sweep can
-- never count the same kill twice for the same entity.
CREATE TABLE IF NOT EXISTS kill_entity_applied (
    killmail_id   BIGINT NOT NULL,
    kind          TEXT NOT NULL,
    entity_id     BIGINT NOT NULL,
    killmail_time TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (killmail_id, kind, entity_id)
);
CREATE INDEX IF NOT EXISTS kill_entity_applied_time_idx ON kill_entity_applied (killmail_time);

-- The board headline, refreshed by the worker pass so a render
-- reads one row plus the stored top lists and nothing else.
CREATE TABLE IF NOT EXISTS killboard_headline (
    id               INTEGER PRIMARY KEY,
    kills_hour       BIGINT NOT NULL DEFAULT 0,
    kills_day        BIGINT NOT NULL DEFAULT 0,
    isk_day          DOUBLE PRECISION NOT NULL DEFAULT 0,
    biggest_killmail_id BIGINT NOT NULL DEFAULT 0,
    computed_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS killboard_top (
    kind      TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    kills     BIGINT NOT NULL DEFAULT 0,
    isk       DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, entity_id)
);
