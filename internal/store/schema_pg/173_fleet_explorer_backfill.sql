-- Step 173: queues once for a rebuild every fleet that holds a battle
-- with no explorer (fleet_battle_detail, step 172), so reports built
-- before the explorer gain one. The worker rebuilds the queued fleets a
-- batch at a time, from the fights it already stores; a battle with no
-- clock steps to read still has no explorer afterwards and keeps the
-- plain report. The demo's fleets are never rebuilt by the worker: the
-- demo reseeds itself when one of its battles lacks an explorer.
INSERT INTO fleet_report_state (op_id, dirty_seq)
SELECT q.op_id, nextval('fleet_report_seq')
FROM (
    SELECT DISTINCT b.op_id
    FROM fleet_battles b
    LEFT JOIN fleet_battle_detail d ON d.fleet_battle_id = b.id
    WHERE d.fleet_battle_id IS NULL
) q
ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq;
