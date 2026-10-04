-- ---------------------------------------------------------------------
-- Structure-name provenance (schema 023, v0.3.08): every cached
-- player-structure name now records where it came from, so a
-- future lower-trust source (a community dataset) can never
-- overwrite a name ESI itself provided. Values: 'esi' (the
-- authenticated /universe/structures/{id}/ lookup, or rows that
-- predate this column), 'corp' (the corporation structures list),
-- 'community' (reserved; plumbed but no dataset ships yet).
-- ---------------------------------------------------------------------
ALTER TABLE structure_names ADD COLUMN source TEXT NOT NULL DEFAULT 'esi';
