-- Step 178: static data from CCP's SDE API. The importer keeps the
-- last copy of each table it read, as the API served it (gzipped JSON
-- Lines), under the table's version. A new release then downloads only
-- the tables whose version changed and reads the rest from here.
CREATE TABLE IF NOT EXISTS sde_api_blobs (
    table_name TEXT PRIMARY KEY,
    version    TEXT        NOT NULL,
    body       BYTEA       NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL
);

-- The per-file markers of the CSV dump the importer used to download
-- have no reader any more.
DELETE FROM sde_meta WHERE key LIKE 'etag:%' OR key LIKE 'last_modified:%';
