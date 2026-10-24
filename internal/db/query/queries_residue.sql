-- The residue step's store (schema 091): completed passes and
-- the notes they leave on a reading. Notes are written at ingest
-- by the residue worker and read by nothing that renders; the
-- review surface is their first reader.

-- name: InsertIntelResidueCall :one
INSERT INTO intel_residue_calls (report_id, spans, output, created_at)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: InsertIntelResidueNote :exec
INSERT INTO intel_residue_notes (report_id, call_id, span, kind, entity_id, entity_name, n, unconfirmed, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListIntelResidueNotes :many
SELECT id, report_id, call_id, span, kind, entity_id, entity_name, n, unconfirmed, created_at
FROM intel_residue_notes
WHERE report_id = $1
ORDER BY created_at, id
LIMIT $2;
