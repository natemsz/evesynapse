-- Killboard statistics, counted once per kill (killboard_stats.go).
--
-- kill_stats_pending lists the public kills not yet counted into the
-- daily aggregates. Ingest adds a kill when it lands public (or
-- turns public); the fold claims it by deleting its row in the same
-- transaction as the aggregates, so a kill is counted exactly once.
-- It replaces kill_entity_applied, the per (kill, entity) marker,
-- which grew to millions of rows for the same guarantee.
CREATE TABLE IF NOT EXISTS kill_stats_pending (
    killmail_id   BIGINT PRIMARY KEY,
    killmail_time TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS kill_stats_pending_time_idx ON kill_stats_pending (killmail_time DESC);

-- The one time rebuild of the daily aggregates from complete kill
-- rows (killboard_rebuild.go): counted in chunks into the _rebuild
-- tables, then swapped in for the live ones. cursor is the last
-- killmail_id counted; done_at marks the swap, after which the
-- retention prune runs.
CREATE TABLE IF NOT EXISTS kill_stats_rebuild_state (
    id         INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    cursor     BIGINT NOT NULL DEFAULT 0,
    started_at TIMESTAMPTZ,
    done_at    TIMESTAMPTZ
);
INSERT INTO kill_stats_rebuild_state (id) VALUES (1) ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS kill_entity_daily_rebuild (
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
    CONSTRAINT kill_entity_daily_rebuild_pkey PRIMARY KEY (day, kind, entity_id)
);

CREATE TABLE IF NOT EXISTS kill_entity_daily_ships_rebuild (
    day          DATE NOT NULL,
    kind         TEXT NOT NULL,
    entity_id    BIGINT NOT NULL,
    ship_type_id BIGINT NOT NULL,
    flown        BIGINT NOT NULL DEFAULT 0,
    lost         BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT kill_entity_daily_ships_rebuild_pkey PRIMARY KEY (day, kind, entity_id, ship_type_id)
);

CREATE TABLE IF NOT EXISTS kill_entity_daily_systems_rebuild (
    day       DATE NOT NULL,
    kind      TEXT NOT NULL,
    entity_id BIGINT NOT NULL,
    system_id BIGINT NOT NULL,
    events    BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT kill_entity_daily_systems_rebuild_pkey PRIMARY KEY (day, kind, entity_id, system_id)
);

DROP TABLE IF EXISTS kill_entity_applied;
