-- Indexes no query reads. The entity boards page through
-- kill_entity_feed and the stats read the daily aggregates, so
-- nothing looks kills up by a victim's corporation or alliance, by
-- an attacker's corporation, or through the involvement record by
-- entity (it is read by killmail id only). Measured on production
-- 2026-11-11: none was scanned; together they held about 5.8 GB
-- that every new kill wrote to. A board feature that needs one
-- back can build it with CREATE INDEX CONCURRENTLY.
DROP INDEX IF EXISTS kill_involvement_char_idx;
DROP INDEX IF EXISTS kill_involvement_corp_idx;
DROP INDEX IF EXISTS kill_involvement_alliance_idx;
DROP INDEX IF EXISTS kill_attackers_corp_idx;
DROP INDEX IF EXISTS kills_victim_corp_idx;
DROP INDEX IF EXISTS kills_victim_alliance_idx;
