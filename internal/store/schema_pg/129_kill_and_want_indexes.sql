-- Kill and history-want indexes (schema 129).
--
-- kills_ingested_idx serves the reads that filter or aggregate on
-- ingested_at: the latest ingest time the map's threat layer
-- checks, the board's recent-name warm every minute, and the
-- ingest count since a moment. Without it each one reads the
-- whole kills table.
--
-- market_history_wants_requested_idx serves the urgent drain's
-- read of recently requested history wants, which runs every few
-- seconds and otherwise reads the whole wants table each time.
CREATE INDEX IF NOT EXISTS kills_ingested_idx ON kills (ingested_at DESC);
CREATE INDEX IF NOT EXISTS market_history_wants_requested_idx ON market_history_wants (last_requested_at DESC);
