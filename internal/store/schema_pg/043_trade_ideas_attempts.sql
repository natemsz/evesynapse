-- Schema step 043: when the trade ideas were last asked for, and how
-- many asks in a row have failed.
--
-- The refresh only knew when an answer last landed, so while the
-- model was failing it asked again on every worker cycle. The last
-- attempt and the run of failures are kept here, and a failing
-- refresh backs off (30 minutes, doubling, up to the 12-hour
-- cadence). One row.

CREATE TABLE IF NOT EXISTS trade_ideas_attempts (
    id           BIGINT PRIMARY KEY CHECK (id = 1),
    attempted_at timestamptz NOT NULL,
    failures     INTEGER NOT NULL DEFAULT 0
);
