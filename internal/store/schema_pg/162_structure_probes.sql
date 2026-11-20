-- Characters that could not name a structure (ESI answered 403 or
-- 404). The structure-name worker asks each eligible character once
-- per structure, across cycles, instead of every character again on
-- every cycle; each refusal counts against ESI's error limit. Once
-- every candidate has been asked the structure settles as missing
-- and its rows are cleared.
CREATE TABLE IF NOT EXISTS structure_probes (
    structure_id BIGINT      NOT NULL,
    character_id BIGINT      NOT NULL,
    asked_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (structure_id, character_id)
);

CREATE INDEX IF NOT EXISTS structure_probes_asked_at ON structure_probes (asked_at);
