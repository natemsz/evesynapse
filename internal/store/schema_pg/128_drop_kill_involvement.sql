-- Kill involvement (schema 128): drop the involvement table.
-- The killboards and My Killmails read zKillboard live now, so
-- the local involvement index is dead weight: 11 GB, most of it
-- index. The kills summary rows and the pilot activity profiles
-- stay; only the link table goes. The drop itself is metadata
-- only and fast; the 11 GB returns to the OS once it commits.
DROP TABLE IF EXISTS kill_involvement;
