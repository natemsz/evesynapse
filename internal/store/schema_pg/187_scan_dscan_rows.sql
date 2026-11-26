-- Schema step 187: a d-scan's own rows, kept beside its reading.
--
-- The report workbench lists each ship type's instances with their
-- own names and distances. Those come from the paste's rows, which
-- are parsed once when the scan is stored and kept here (type id,
-- name, distance; JSON), so a page view never parses the raw paste
-- again. NULL for a paste that is not a d-scan, and for scans
-- stored before this step (the workbench reads those the old way).

ALTER TABLE scans
    ADD COLUMN IF NOT EXISTS dscan_rows TEXT;
