-- Admin analytics (097): the recorder's stored aggregates. The
-- worker flushes in-memory counters into these tables once a
-- minute (plus one row per finished worker cycle); the Admin
-- page's Analytics section reads nothing else for its charts.
-- Minute rows live 7 days, then roll up into the hourly tables,
-- which live 90 days. Nothing here identifies an account: route
-- groups are route templates and event kinds are fixed words.

CREATE TABLE IF NOT EXISTS stats_requests_minute (
    minute       TIMESTAMPTZ NOT NULL,
    route_group  TEXT        NOT NULL,
    status_class SMALLINT    NOT NULL,
    requests     BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, route_group, status_class)
);

CREATE TABLE IF NOT EXISTS stats_requests_hourly (
    hour         TIMESTAMPTZ NOT NULL,
    route_group  TEXT        NOT NULL,
    status_class SMALLINT    NOT NULL,
    requests     BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, route_group, status_class)
);

-- Response-time histograms: bucket is an index into the edge list
-- the recorder and the page share (stats_recorder.go).
CREATE TABLE IF NOT EXISTS stats_durations_minute (
    minute      TIMESTAMPTZ NOT NULL,
    route_group TEXT        NOT NULL,
    bucket      SMALLINT    NOT NULL,
    hits        BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, route_group, bucket)
);

CREATE TABLE IF NOT EXISTS stats_durations_hourly (
    hour        TIMESTAMPTZ NOT NULL,
    route_group TEXT        NOT NULL,
    bucket      SMALLINT    NOT NULL,
    hits        BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, route_group, bucket)
);

CREATE TABLE IF NOT EXISTS stats_esi_minute (
    minute TIMESTAMPTZ NOT NULL,
    family TEXT        NOT NULL,
    calls  BIGINT      NOT NULL DEFAULT 0,
    errors BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, family)
);

CREATE TABLE IF NOT EXISTS stats_esi_hourly (
    hour   TIMESTAMPTZ NOT NULL,
    family TEXT        NOT NULL,
    calls  BIGINT      NOT NULL DEFAULT 0,
    errors BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, family)
);

-- Site events with no owning table of their own (sign-ins,
-- contract searches), counted by the recorder like the rest.
CREATE TABLE IF NOT EXISTS stats_events_minute (
    minute TIMESTAMPTZ NOT NULL,
    kind   TEXT        NOT NULL,
    count  BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (minute, kind)
);

CREATE TABLE IF NOT EXISTS stats_events_hourly (
    hour  TIMESTAMPTZ NOT NULL,
    kind  TEXT        NOT NULL,
    count BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, kind)
);

CREATE TABLE IF NOT EXISTS stats_host_minute (
    minute           TIMESTAMPTZ PRIMARY KEY,
    load1            DOUBLE PRECISION NOT NULL DEFAULT 0,
    load5            DOUBLE PRECISION NOT NULL DEFAULT 0,
    load15           DOUBLE PRECISION NOT NULL DEFAULT 0,
    mem_used_bytes   BIGINT NOT NULL DEFAULT 0,
    mem_total_bytes  BIGINT NOT NULL DEFAULT 0,
    app_rss_bytes    BIGINT NOT NULL DEFAULT 0,
    goroutines       BIGINT NOT NULL DEFAULT 0,
    disk_used_bytes  BIGINT NOT NULL DEFAULT 0,
    disk_total_bytes BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS stats_host_hourly (
    hour             TIMESTAMPTZ PRIMARY KEY,
    samples          BIGINT NOT NULL DEFAULT 0,
    load1_sum        DOUBLE PRECISION NOT NULL DEFAULT 0,
    load5_sum        DOUBLE PRECISION NOT NULL DEFAULT 0,
    load15_sum       DOUBLE PRECISION NOT NULL DEFAULT 0,
    mem_used_sum     BIGINT NOT NULL DEFAULT 0,
    mem_used_max     BIGINT NOT NULL DEFAULT 0,
    mem_total_bytes  BIGINT NOT NULL DEFAULT 0,
    app_rss_sum      BIGINT NOT NULL DEFAULT 0,
    goroutines_max   BIGINT NOT NULL DEFAULT 0,
    disk_used_bytes  BIGINT NOT NULL DEFAULT 0,
    disk_total_bytes BIGINT NOT NULL DEFAULT 0
);

-- One row per finished worker cycle, with its phase timings in
-- the child table under the names the cycle clock already uses.
CREATE TABLE IF NOT EXISTS stats_cycles (
    at         TIMESTAMPTZ PRIMARY KEY,
    took_ms    BIGINT NOT NULL DEFAULT 0,
    fetches    BIGINT NOT NULL DEFAULT 0,
    budget     BIGINT NOT NULL DEFAULT 0,
    deferred   BIGINT NOT NULL DEFAULT 0,
    characters BIGINT NOT NULL DEFAULT 0,
    esi_state  TEXT   NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS stats_cycle_phases (
    cycle_at TIMESTAMPTZ NOT NULL,
    phase    TEXT        NOT NULL,
    took_ms  BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (cycle_at, phase)
);
CREATE INDEX IF NOT EXISTS stats_cycle_phases_at_idx ON stats_cycle_phases (cycle_at);

CREATE TABLE IF NOT EXISTS stats_cycles_hourly (
    hour          TIMESTAMPTZ PRIMARY KEY,
    cycles        BIGINT NOT NULL DEFAULT 0,
    took_sum_ms   BIGINT NOT NULL DEFAULT 0,
    took_max_ms   BIGINT NOT NULL DEFAULT 0,
    fetches_sum   BIGINT NOT NULL DEFAULT 0,
    deferred_sum  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS stats_cycle_phases_hourly (
    hour       TIMESTAMPTZ NOT NULL,
    phase      TEXT        NOT NULL,
    took_sum_ms BIGINT     NOT NULL DEFAULT 0,
    samples    BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, phase)
);

-- Hourly data-inventory snapshot: the largest tables by size,
-- plus one row with an empty name for the database itself.
CREATE TABLE IF NOT EXISTS stats_inventory (
    hour       TIMESTAMPTZ NOT NULL,
    name       TEXT        NOT NULL,
    rows_est   BIGINT      NOT NULL DEFAULT 0,
    size_bytes BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, name)
);

-- The Activity panel counts these tables by window; give the
-- counts an index each so the render stays a fixed small cost.
CREATE INDEX IF NOT EXISTS intel_reports_created_idx ON intel_reports (created_at);
CREATE INDEX IF NOT EXISTS user_appraisals_created_idx ON user_appraisals (created_at);
CREATE INDEX IF NOT EXISTS local_fittings_created_idx ON local_fittings (created_at);
