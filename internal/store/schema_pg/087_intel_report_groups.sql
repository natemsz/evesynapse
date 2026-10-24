-- Intel report groups (schema 087): channel reports on one
-- system inside a sitting belong to one group, as scans do
-- (schema 086). group_id is the founding report's own id: a
-- report that starts a group is named for itself, and a group's
-- composite picture (below) is keyed by the same id. Reports
-- stored before groups name themselves, which is what the
-- backfill sets; group_id 0 means grouping has not run on the
-- row (an insert in flight).
ALTER TABLE intel_reports ADD COLUMN IF NOT EXISTS group_id BIGINT NOT NULL DEFAULT 0;
UPDATE intel_reports SET group_id = id WHERE group_id = 0;
CREATE INDEX IF NOT EXISTS intel_reports_group_idx ON intel_reports (group_id, seen_at);
CREATE INDEX IF NOT EXISTS intel_reports_channel_system_idx ON intel_reports (channel_id, system_id, seen_at DESC);

-- A group's composite: the fused picture of its member
-- readings, computed at ingest when a report founds or joins
-- the group and read whole by every render (the workbench,
-- the feed cards). previous keeps the composite as it stood
-- before the latest member landed, for the diff view.
CREATE TABLE IF NOT EXISTS intel_group_composites (
    group_id    BIGINT PRIMARY KEY,
    channel_id  BIGINT NOT NULL,
    system_id   BIGINT NOT NULL,
    composite   TEXT NOT NULL DEFAULT '',
    previous    TEXT NOT NULL DEFAULT '',
    members     INTEGER NOT NULL DEFAULT 0,
    computed_at TIMESTAMPTZ NOT NULL
);
