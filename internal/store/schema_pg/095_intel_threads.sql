-- Intel threads (schema 095, intel_threads.go): a channel's report
-- groups on one system or on neighbouring ones, close together in
-- time, read as one engagement. A group (schema 087) stays one
-- system's picture; a thread links groups as a gang moves through
-- gates. thread_id is the founding group's own id, as a group is
-- named for its founding report; latest_at is the group's latest
-- member moment, kept beside the composite so a thread's
-- candidates are one indexed read. Groups stored before threads
-- each found their own.
ALTER TABLE intel_group_composites ADD COLUMN IF NOT EXISTS thread_id BIGINT NOT NULL DEFAULT 0;
ALTER TABLE intel_group_composites ADD COLUMN IF NOT EXISTS latest_at TIMESTAMPTZ;
UPDATE intel_group_composites SET thread_id = group_id WHERE thread_id = 0;
UPDATE intel_group_composites SET latest_at = computed_at WHERE latest_at IS NULL;
CREATE INDEX IF NOT EXISTS intel_group_composites_channel_latest_idx ON intel_group_composites (channel_id, latest_at DESC);
CREATE INDEX IF NOT EXISTS intel_group_composites_thread_idx ON intel_group_composites (thread_id);
