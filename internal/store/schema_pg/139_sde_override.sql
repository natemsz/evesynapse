-- Schema step 139: an uploaded static data dump.
--
-- An admin can upload a zip of dump CSV files for the importer to
-- read in place of the regular dump, for the days when the game has
-- changed and the regular dump has not caught up. One row at most.
-- The importer drops the row when an import from it fails, and when
-- the regular dump changes after the upload.
CREATE TABLE IF NOT EXISTS sde_override (
    id          SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    label       TEXT        NOT NULL,
    archive     BYTEA       NOT NULL,
    sha256      TEXT        NOT NULL,
    size_bytes  BIGINT      NOT NULL,
    files       TEXT        NOT NULL,
    uploaded_by BIGINT      NOT NULL DEFAULT 0,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
