-- Axon submission labels (schema 094, intel_preview.go): a
-- submission the player sent with an override -- the scan type
-- or the system set by hand in Axon's overlay -- logged beside
-- what the site detected for the same text. Each row is a labelled
-- example for the classifier the residue corpus is building
-- toward: the player's word against the reading's. Rows are
-- written once and never read back by the site itself.
CREATE TABLE IF NOT EXISTS intel_submit_labels (
    id                 BIGSERIAL PRIMARY KEY,
    channel_id         BIGINT NOT NULL,
    user_id            BIGINT NOT NULL,
    raw                TEXT NOT NULL,
    detected_kind      TEXT NOT NULL DEFAULT '',
    detected_system_id BIGINT NOT NULL DEFAULT 0,
    override_kind      TEXT NOT NULL DEFAULT '',
    override_system_id BIGINT NOT NULL DEFAULT 0,
    posted             INTEGER NOT NULL DEFAULT 0,
    created_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS intel_submit_labels_created ON intel_submit_labels (created_at);
