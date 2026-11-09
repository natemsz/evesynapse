-- Kill entity feed (schema 132): one slim row per (entity, kill)
-- for every character, corporation and alliance on a public kill,
-- victim side included, written once at ingest beside the daily
-- aggregates (killboard_stats.go). The entity boards page through
-- this table alone: a single keyset scan on (kind, entity, time,
-- id), so a page costs the same for an alliance with ten kills
-- and one with ten million, and no kill is ever re-read to build
-- a listing. Rows are permanent, like the involvement record;
-- the value column is the kill's priced value (value_ours, 0
-- until priced) and solo its flag, so the board's filters ride
-- the same scan instead of re-checking the kills table.

CREATE TABLE IF NOT EXISTS kill_entity_feed (
    kind          TEXT NOT NULL,
    entity_id     BIGINT NOT NULL,
    killmail_time TIMESTAMPTZ NOT NULL,
    killmail_id   BIGINT NOT NULL,
    solo          BOOLEAN NOT NULL DEFAULT FALSE,
    value         DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (kind, entity_id, killmail_id)
);
CREATE INDEX IF NOT EXISTS kill_entity_feed_page_idx
    ON kill_entity_feed (kind, entity_id, killmail_time DESC, killmail_id DESC);
