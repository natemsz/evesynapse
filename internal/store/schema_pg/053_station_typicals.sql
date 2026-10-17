-- Schema step 053: station-grain typical prices.
--
-- The region sweeps distilled honest prices (the median order
-- per side) at region grain only; a station's row kept just its
-- best price per side, so the spread scanner priced its rows
-- off whatever orders happened to be alone at a station -- one
-- fishing bid under one hopeful ask printed as a 2000% spread.
-- The sweep now distills the median per station and type too,
-- and the scanner prices typical against typical, the same
-- basis the tradefinder routes on. The best prices stay stored
-- beside them; nothing that read them changes.

ALTER TABLE market_station_stats ADD COLUMN IF NOT EXISTS typical_sell DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE market_station_stats ADD COLUMN IF NOT EXISTS typical_buy DOUBLE PRECISION NOT NULL DEFAULT 0;
