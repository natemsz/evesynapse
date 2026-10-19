-- Schema step 063: the SDE's mutaplasmid applicability
-- mappings (dynamicItemAttributeMappings), imported by the
-- SDE importer alongside the roll ranges (schema 055's
-- abyssal_ranges) and replaced on each import that carries
-- the file; sde_build is that file's marker.
--
-- One row per (mutaplasmid, base type it can be applied to);
-- resulting_type_id is the type an item of the base type
-- becomes. The public-contract probe queue reads the
-- resulting types: only an instance whose type a mutaplasmid
-- can produce can ever answer a dogma probe, so every other
-- singleton stays out of the queue instead of spending probe
-- slots and ESI error budget on a guaranteed 404.

CREATE TABLE IF NOT EXISTS abyssal_mappings (
    mutator_type_id    BIGINT NOT NULL,
    applicable_type_id BIGINT NOT NULL,
    resulting_type_id  BIGINT NOT NULL,
    sde_build          TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (mutator_type_id, applicable_type_id)
);
CREATE INDEX IF NOT EXISTS abyssal_mappings_resulting_idx
    ON abyssal_mappings (resulting_type_id);
