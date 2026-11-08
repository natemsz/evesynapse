-- Station owners (schema 130).
--
-- sde_stations gains the owning NPC corporation, so the fee
-- math can look up the station's owner and apply the
-- character's standings with that corporation and its
-- faction. sde_npc_corporations carries the corporation to
-- faction map from the SDE's crpNPCCorporations.csv. Both
-- tables refill on the next SDE sync; until then the owner
-- reads 0 and the fee math treats the station as unknown.
ALTER TABLE sde_stations ADD COLUMN IF NOT EXISTS corporation_id BIGINT NOT NULL DEFAULT 0;
CREATE TABLE IF NOT EXISTS sde_npc_corporations (
	corporation_id BIGINT PRIMARY KEY,
	faction_id BIGINT NOT NULL DEFAULT 0
);
