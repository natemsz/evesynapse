-- Known hostiles (schema 080): pilots an intel channel watches
-- (internal/app/pilot_intel.go). Anyone who may post to the channel
-- adds or removes one, with a short note; everyone who reads the
-- channel sees the list, on the channel page, the pilot's page and
-- the map's "Watched pilots" layer.
CREATE TABLE IF NOT EXISTS intel_watch (
    channel_id    BIGINT NOT NULL REFERENCES intel_channels (id) ON DELETE CASCADE,
    character_id  BIGINT NOT NULL,
    note          TEXT NOT NULL DEFAULT '',
    added_by      BIGINT NOT NULL DEFAULT 0,
    added_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (channel_id, character_id)
);
CREATE INDEX IF NOT EXISTS intel_watch_character_idx ON intel_watch (character_id);
