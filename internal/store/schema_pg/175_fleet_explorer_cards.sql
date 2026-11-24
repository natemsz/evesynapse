-- Step 175: queues once for a rebuild every fleet that holds a battle,
-- so its explorer gains what the hover cards and the moment panel read:
-- each pilot's and enemy's heaviest weapons, a small chart of each
-- moment's window, and the chart's card for a battle drawn from its
-- killmails. The worker rebuilds the queued fleets a batch at a time
-- from what it already stores. The demo's fleets are never rebuilt by
-- the worker; the demo seeds its own.
INSERT INTO fleet_report_state (op_id, dirty_seq)
SELECT q.op_id, nextval('fleet_report_seq')
FROM (SELECT DISTINCT op_id FROM fleet_battles) q
ON CONFLICT (op_id) DO UPDATE SET dirty_seq = excluded.dirty_seq;
