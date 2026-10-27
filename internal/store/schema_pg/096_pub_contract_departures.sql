-- Public contract departures (schema 096, pub_contracts.go): a
-- completed sweep no longer deletes the contracts that left. The
-- row stays, with its items and bids, stamped with when it left
-- and how ('expired', or 'vanished' early: the same depart_kind
-- the abyssal sighting stamp carries), and last_seen keeps the
-- last sweep that saw it. Departed rows are pruned after a
-- retention window. Every live reader filters on departed_at IS
-- NULL, so the stored departures are evidence only and never
-- show as open contracts.
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS departed_at TIMESTAMPTZ;
ALTER TABLE pub_contracts ADD COLUMN IF NOT EXISTS depart_kind TEXT;
-- The prune walks one region's departures older than the window.
CREATE INDEX IF NOT EXISTS pub_contracts_departed_idx
    ON pub_contracts (region_id, departed_at) WHERE departed_at IS NOT NULL;
