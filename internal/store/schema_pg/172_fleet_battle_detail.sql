-- Schema step 172: a fleet battle's explorer.
--
-- One row per fleet battle, written by the report worker with the battle
-- (fleet_explorer.go): its steps, each pilot's lane, the enemies merged
-- across logs, the weapons and tackle, the moments of its timeline with
-- what happened in each, and what the numbers say. The report page reads
-- this one row.
CREATE TABLE IF NOT EXISTS fleet_battle_detail (
    fleet_battle_id BIGINT PRIMARY KEY REFERENCES fleet_battles (id) ON DELETE CASCADE,
    payload         TEXT NOT NULL
);
