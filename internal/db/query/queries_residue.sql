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

-- The backlog sweep's candidates (residue_model.go): settled
-- reports whose stored reading left residue and which no
-- completed pass has covered, newest first. The CASE keeps the
-- jsonb read off rows whose analysis was never stored.
-- name: ListResidueBacklogReports :many
SELECT r.id
FROM intel_reports r
WHERE r.analysis_pending = false
  AND CASE WHEN r.analysis <> '' THEN
        jsonb_typeof(r.analysis::jsonb -> 'residue') = 'array'
        AND jsonb_array_length(r.analysis::jsonb -> 'residue') > 0
      ELSE false END
  AND NOT EXISTS (SELECT 1 FROM intel_residue_calls c WHERE c.report_id = r.id)
ORDER BY r.id DESC
LIMIT $1;
