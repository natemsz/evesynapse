-- Per-line prices for a kill (schema 093, killfeed.go): each
-- kill's lines are priced once, at ingest (and by the pricing
-- pass for kills held from before), and the kill page reads
-- the stored unit prices instead of repricing per request.
CREATE TABLE IF NOT EXISTS kill_line_prices (
    killmail_id BIGINT NOT NULL,
    type_id     BIGINT NOT NULL,
    unit_price  DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (killmail_id, type_id)
);
