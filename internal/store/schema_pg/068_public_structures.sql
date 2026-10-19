-- Schema step 068: the public structures set.
--
-- ESI's GET /universe/structures/ route lists every player
-- structure that is publicly dockable. The contract worker
-- mirrors that list here on the contract sweep cadence (one
-- replace per refresh, stamped), so the contract search's
-- un-dockable-citadel filter reads a stored set instead of
-- asking ESI at render. A structure absent from this table
-- is not public; NPC stations never appear here at all --
-- they live in sde_stations and are always dockable.

CREATE TABLE IF NOT EXISTS public_structures (
    structure_id BIGINT PRIMARY KEY,
    refreshed_at TIMESTAMPTZ NOT NULL
);
