-- Migration 016: pilot-record drain priority. A pilot page a user
-- actually opened outranks the proactively noted orbit of strangers
-- they have dealt with, so ListPilotDrains serves viewed wants
-- first. 1 = viewed (or bumped by a view), 0 = orbit-noted.
ALTER TABLE pilot_records ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
