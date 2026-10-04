-- Migration 018 (current-page urgency): bulk-cache EVE item
-- descriptions from the SDE invTypes dump alongside the type
-- names, so item pages read flavor text locally instead of
-- waiting on a per-type ESI fetch. Guarded in db.go on the
-- column itself, like 008/009.
ALTER TABLE sde_types ADD COLUMN description TEXT NOT NULL DEFAULT '';
