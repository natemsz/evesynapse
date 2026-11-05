-- Admin analytics (125): the worker's backlog depths, one row
-- per finished cycle per backlog, recorded by the stats flush
-- (stats_recorder.go) and read by the Admin page's ESI Details
-- tab. Depths are gauges, not counters: each row is the depth
-- the worker measured at the end of that cycle. Minute-table
-- retention matches the other cycle series: raw rows live 7
-- days, then roll into the hourly table, kept 90 days.

CREATE TABLE IF NOT EXISTS stats_backlogs (
    cycle_at TIMESTAMPTZ NOT NULL,
    backlog  TEXT        NOT NULL,
    depth    BIGINT      NOT NULL DEFAULT 0,
    PRIMARY KEY (cycle_at, backlog)
);
CREATE INDEX IF NOT EXISTS stats_backlogs_kind_idx ON stats_backlogs (backlog, cycle_at DESC);

CREATE TABLE IF NOT EXISTS stats_backlogs_hourly (
    hour      TIMESTAMPTZ NOT NULL,
    backlog   TEXT        NOT NULL,
    depth_avg DOUBLE PRECISION NOT NULL DEFAULT 0,
    depth_max BIGINT NOT NULL DEFAULT 0,
    samples   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (hour, backlog)
);
