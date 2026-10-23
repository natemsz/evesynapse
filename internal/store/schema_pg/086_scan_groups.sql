-- Scan groups (schema 086): a paste can join a group of scans --
-- the next d-scan five minutes later, the local taken alongside
-- it (internal/app/scans.go). group_id is the founding scan's
-- own id: a fresh paste starts a group named for itself, and a
-- group lives on its members' scan pages. Scans stored before
-- groups keep group_id '' and belong to no group.
ALTER TABLE scans ADD COLUMN IF NOT EXISTS group_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS scans_group_idx ON scans (group_id, created_at) WHERE group_id <> '';
CREATE INDEX IF NOT EXISTS scans_system_idx ON scans (system_id, created_at DESC) WHERE system_id > 0;
