-- Schema step 055: public contracts (the search corpus), the
-- abyssal item corpus, and the mutaplasmid roll ranges.
--
-- The worker mirrors ESI's public contract routes into these
-- tables on the routes' own cache cadence; pages (a later step)
-- read stored rows only. The contract tables hold only LIVE
-- contracts: a row leaves when the contract expires or vanishes
-- from two consecutive sweeps of its region. The abyssal
-- tables never lose rows -- an item instance's rolls and its
-- listing history are permanent by design, because they are
-- the anchor data any abyssal pricing math calibrates against,
-- and history that was not captured cannot be re-collected.

CREATE TABLE IF NOT EXISTS pub_contracts (
    contract_id         BIGINT PRIMARY KEY,
    region_id           BIGINT NOT NULL,
    type                TEXT NOT NULL,
    issuer_id           BIGINT NOT NULL DEFAULT 0,
    issuer_corporation_id BIGINT NOT NULL DEFAULT 0,
    for_corporation     BOOLEAN NOT NULL DEFAULT FALSE,
    title               TEXT NOT NULL DEFAULT '',
    price               DOUBLE PRECISION NOT NULL DEFAULT 0,
    buyout              DOUBLE PRECISION NOT NULL DEFAULT 0,
    reward              DOUBLE PRECISION NOT NULL DEFAULT 0,
    collateral          DOUBLE PRECISION NOT NULL DEFAULT 0,
    volume              DOUBLE PRECISION NOT NULL DEFAULT 0,
    start_location_id   BIGINT NOT NULL DEFAULT 0,
    end_location_id     BIGINT NOT NULL DEFAULT 0,
    date_issued         TIMESTAMPTZ NOT NULL,
    date_expired        TIMESTAMPTZ NOT NULL,
    days_to_complete    BIGINT NOT NULL DEFAULT 0,
    -- pending: not tried yet; stored: item rows are in;
    -- private: ESI refuses the list (courier cargo); gone: the
    -- contract ended before its list was ever fetched.
    items_state         TEXT NOT NULL DEFAULT 'pending',
    -- The region sweep cycle that last saw this contract
    -- (pub_contract_sweep_state.cycle of its region).
    last_cycle          BIGINT NOT NULL DEFAULT 0,
    first_seen          TIMESTAMPTZ NOT NULL,
    last_seen           TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS pub_contracts_region_expiry_idx
    ON pub_contracts (region_id, date_expired);
CREATE INDEX IF NOT EXISTS pub_contracts_issuer_idx
    ON pub_contracts (issuer_id);
CREATE INDEX IF NOT EXISTS pub_contracts_price_idx
    ON pub_contracts (price);

CREATE TABLE IF NOT EXISTS pub_contract_items (
    record_id           BIGINT PRIMARY KEY,
    contract_id         BIGINT NOT NULL,
    type_id             BIGINT NOT NULL,
    -- Present only for singleton (unpackaged, unique) items.
    item_id             BIGINT,
    quantity            BIGINT NOT NULL DEFAULT 0,
    is_included         BOOLEAN NOT NULL DEFAULT TRUE,
    is_blueprint_copy   BOOLEAN NOT NULL DEFAULT FALSE,
    runs                BIGINT,
    material_efficiency BIGINT,
    time_efficiency     BIGINT
);
CREATE INDEX IF NOT EXISTS pub_contract_items_contract_idx
    ON pub_contract_items (contract_id);
CREATE INDEX IF NOT EXISTS pub_contract_items_type_idx
    ON pub_contract_items (type_id, is_included);
CREATE INDEX IF NOT EXISTS pub_contract_items_item_idx
    ON pub_contract_items (item_id) WHERE item_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS pub_contract_bids (
    bid_id      BIGINT PRIMARY KEY,
    contract_id BIGINT NOT NULL,
    amount      DOUBLE PRECISION NOT NULL DEFAULT 0,
    date_bid    TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS pub_contract_bids_contract_idx
    ON pub_contract_bids (contract_id);

-- One row per region that has ever swept: the resumable
-- cursor (next_page > 0 while a sweep is between pages) and
-- the count of completed sweeps, which contract rows carry
-- as last_cycle for the two-missed-cycles death rule.
CREATE TABLE IF NOT EXISTS pub_contract_sweep_state (
    region_id   BIGINT PRIMARY KEY,
    next_page   BIGINT NOT NULL DEFAULT 0,
    total_pages BIGINT NOT NULL DEFAULT 0,
    cycle       BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL
);

-- One row per abyssal (mutaplasmid-rolled) item instance,
-- fetched once ever from ESI's dogma dynamic route: rolls
-- never change, so the row never expires. attributes holds
-- the route's dogma_attributes array verbatim.
CREATE TABLE IF NOT EXISTS abyssal_items (
    item_id        BIGINT PRIMARY KEY,
    type_id        BIGINT NOT NULL,
    source_type_id BIGINT NOT NULL DEFAULT 0,
    mutator_type_id BIGINT NOT NULL DEFAULT 0,
    created_by     BIGINT NOT NULL DEFAULT 0,
    attributes     JSONB NOT NULL DEFAULT '[]',
    fetched_at     TIMESTAMPTZ NOT NULL,
    first_seen_at  TIMESTAMPTZ NOT NULL,
    last_seen_at   TIMESTAMPTZ NOT NULL
);

-- Every dogma probe's outcome, hit or miss, so no item
-- instance is ever probed twice (misses cost ESI error
-- budget; ordinary modules are the common case).
CREATE TABLE IF NOT EXISTS abyssal_probes (
    item_id    BIGINT PRIMARY KEY,
    type_id    BIGINT NOT NULL,
    is_dynamic BOOLEAN NOT NULL DEFAULT FALSE,
    probed_at  TIMESTAMPTZ NOT NULL
);

-- One row per (abyssal item, contract that listed it): the
-- listing's asking price, region and location, how long the
-- listing was seen live, and how it left the live set
-- (expired = its expiry passed; vanished = it left early --
-- accepted, cancelled or deleted; the corpus records the
-- fact, never a guess at which). The sighting also keeps the
-- listing's own term (date_expired) and, for auctions, the
-- bid state at exit (bid_count, top_bid, snapshotted in the
-- completion step before the bids go with the contract):
-- how early a vanish was, and whether anyone had bid, are
-- exit facts no later pass could reconstruct. A relist is
-- the same instance opening a new sighting on a new
-- contract; the sequence per instance is the whole history.
CREATE TABLE IF NOT EXISTS abyssal_sightings (
    item_id       BIGINT NOT NULL,
    contract_id   BIGINT NOT NULL,
    contract_type TEXT NOT NULL DEFAULT '',
    price         DOUBLE PRECISION NOT NULL DEFAULT 0,
    region_id     BIGINT NOT NULL DEFAULT 0,
    location_id   BIGINT NOT NULL DEFAULT 0,
    date_expired  TIMESTAMPTZ,
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    departed_at   TIMESTAMPTZ,
    depart_kind   TEXT,
    bid_count     BIGINT,
    top_bid       DOUBLE PRECISION,
    PRIMARY KEY (item_id, contract_id)
);
CREATE INDEX IF NOT EXISTS abyssal_sightings_contract_idx
    ON abyssal_sightings (contract_id);

-- The roll ranges per mutaplasmid and attribute, imported
-- from the SDE's dynamicItemAttributes dataset by the SDE
-- importer (which replaces the rows on each import that
-- carries the file); sde_build is that file's marker.
CREATE TABLE IF NOT EXISTS abyssal_ranges (
    mutator_type_id BIGINT NOT NULL,
    attribute_id    BIGINT NOT NULL,
    min_mult        DOUBLE PRECISION NOT NULL,
    max_mult        DOUBLE PRECISION NOT NULL,
    sde_build       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (mutator_type_id, attribute_id)
);

-- Per-family scoring weights for abyssal appraisal: data,
-- seeded here, tunable without code. Family names are the
-- SDE group names of the mutated (resulting) types, slugged
-- (warp tackle shares group 52, "Warp Scrambler", for both
-- disruptors and scramblers, and one row set covers both:
-- they roll the same attributes for the same stakes). A
-- weight is the attribute's voice in its family's score;
-- penalty_only attributes can only pull a score down (the
-- fitting taxes: a good CPU roll adds little, a bad one
-- gates the fit). Direction of "good" is not stored here:
-- it comes from sde_attribute_types.high_is_good.
CREATE TABLE IF NOT EXISTS abyssal_family_weights (
    family       TEXT NOT NULL,
    attribute_id BIGINT NOT NULL,
    weight       DOUBLE PRECISION NOT NULL DEFAULT 0,
    penalty_only BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (family, attribute_id)
);

INSERT INTO abyssal_family_weights (family, attribute_id, weight, penalty_only) VALUES
    -- Ancillary armor repairer: amount per cycle rules; cycle
    -- time next; reload downtime and cap are real but minor.
    ('ancillary_armor_repairer', 84, 1.0, FALSE),
    ('ancillary_armor_repairer', 73, 0.5, FALSE),
    ('ancillary_armor_repairer', 1795, 0.25, FALSE),
    ('ancillary_armor_repairer', 6, 0.25, FALSE),
    ('ancillary_armor_repairer', 50, 0.5, TRUE),
    ('ancillary_armor_repairer', 30, 0.5, TRUE),
    -- Ancillary shield booster: same shape as its armor kin.
    ('ancillary_shield_booster', 68, 1.0, FALSE),
    ('ancillary_shield_booster', 73, 0.5, FALSE),
    ('ancillary_shield_booster', 1795, 0.25, FALSE),
    ('ancillary_shield_booster', 6, 0.25, FALSE),
    ('ancillary_shield_booster', 50, 0.5, TRUE),
    ('ancillary_shield_booster', 30, 0.5, TRUE),
    -- Armor plate: raw HP rules; a light plate is worth real
    -- money, so mass counts; fitting is the usual tax.
    ('armor_plate', 1159, 1.0, FALSE),
    ('armor_plate', 796, 0.5, FALSE),
    ('armor_plate', 50, 0.5, TRUE),
    ('armor_plate', 30, 0.5, TRUE),
    -- Armor repairer: amount, then cycle time, then cap.
    ('armor_repair_unit', 84, 1.0, FALSE),
    ('armor_repair_unit', 73, 0.5, FALSE),
    ('armor_repair_unit', 6, 0.25, FALSE),
    ('armor_repair_unit', 50, 0.5, TRUE),
    ('armor_repair_unit', 30, 0.5, TRUE),
    -- Damage controls: the four hull resonances are the
    -- product, in equal measure.
    ('damage_control', 974, 0.5, FALSE),
    ('damage_control', 975, 0.5, FALSE),
    ('damage_control', 976, 0.5, FALSE),
    ('damage_control', 977, 0.5, FALSE),
    ('damage_control', 73, 0.25, FALSE),
    ('damage_control', 50, 0.5, TRUE),
    -- Missile damage mod: damage first, rate of fire second.
    ('ballistic_control_system', 213, 1.0, FALSE),
    ('ballistic_control_system', 204, 0.5, FALSE),
    ('ballistic_control_system', 50, 0.5, TRUE),
    -- Turret and vorton damage mods: one shape, three houses
    -- (gyrostabilizer, heat sink, magnetic field stabilizer,
    -- entropic radiation sink, vorton projector upgrade).
    ('gyrostabilizer', 64, 1.0, FALSE),
    ('gyrostabilizer', 204, 0.5, FALSE),
    ('gyrostabilizer', 50, 0.5, TRUE),
    ('heat_sink', 64, 1.0, FALSE),
    ('heat_sink', 204, 0.5, FALSE),
    ('heat_sink', 50, 0.5, TRUE),
    ('magnetic_field_stabilizer', 64, 1.0, FALSE),
    ('magnetic_field_stabilizer', 204, 0.5, FALSE),
    ('magnetic_field_stabilizer', 50, 0.5, TRUE),
    ('entropic_radiation_sink', 64, 1.0, FALSE),
    ('entropic_radiation_sink', 204, 0.5, FALSE),
    ('entropic_radiation_sink', 50, 0.5, TRUE),
    ('vorton_projector_upgrade', 64, 1.0, FALSE),
    ('vorton_projector_upgrade', 204, 0.5, FALSE),
    ('vorton_projector_upgrade', 50, 0.5, TRUE),
    -- Capacitor battery: capacity is the product; the
    -- warfare-resistance roll is a curiosity beside it.
    ('capacitor_battery', 67, 1.0, FALSE),
    ('capacitor_battery', 2267, 0.25, FALSE),
    ('capacitor_battery', 50, 0.5, TRUE),
    ('capacitor_battery', 30, 0.5, TRUE),
    -- Combat drones: damage rules, then tracking and speed;
    -- tank rolls are why one drone outlives another.
    ('combat_drone', 64, 1.0, FALSE),
    ('combat_drone', 160, 0.5, FALSE),
    ('combat_drone', 37, 0.5, FALSE),
    ('combat_drone', 9, 0.25, FALSE),
    ('combat_drone', 263, 0.25, FALSE),
    ('combat_drone', 265, 0.25, FALSE),
    ('combat_drone', 54, 0.25, FALSE),
    ('combat_drone', 158, 0.25, FALSE),
    -- Drone support: each module sells exactly one number.
    ('drone_control_range_module', 459, 1.0, FALSE),
    ('drone_control_range_module', 50, 0.5, TRUE),
    ('drone_damage_modules', 1255, 1.0, FALSE),
    ('drone_damage_modules', 50, 0.5, TRUE),
    ('drone_navigation_computer', 20, 1.0, FALSE),
    ('drone_navigation_computer', 50, 0.5, TRUE),
    ('drone_tracking_enhancer', 767, 1.0, FALSE),
    ('drone_tracking_enhancer', 351, 0.5, FALSE),
    ('drone_tracking_enhancer', 349, 0.5, FALSE),
    ('drone_tracking_enhancer', 847, 0.25, FALSE),
    ('drone_tracking_enhancer', 848, 0.25, FALSE),
    ('drone_tracking_enhancer', 50, 0.5, TRUE),
    ('drone_tracking_modules', 767, 1.0, FALSE),
    ('drone_tracking_modules', 351, 0.5, FALSE),
    ('drone_tracking_modules', 349, 0.5, FALSE),
    ('drone_tracking_modules', 847, 0.25, FALSE),
    ('drone_tracking_modules', 848, 0.25, FALSE),
    ('drone_tracking_modules', 50, 0.5, TRUE),
    -- Energy neutralizer: amount drained, then reach.
    ('energy_neutralizer', 97, 1.0, FALSE),
    ('energy_neutralizer', 54, 0.5, FALSE),
    ('energy_neutralizer', 6, 0.25, FALSE),
    ('energy_neutralizer', 50, 0.5, TRUE),
    ('energy_neutralizer', 30, 0.5, TRUE),
    -- Energy nosferatu: amount transferred, then reach.
    ('energy_nosferatu', 90, 1.0, FALSE),
    ('energy_nosferatu', 54, 0.5, FALSE),
    ('energy_nosferatu', 50, 0.5, TRUE),
    ('energy_nosferatu', 30, 0.5, TRUE),
    -- Fighter support unit: the three fighter bonuses share
    -- the stage; recharge trails.
    ('fighter_support_unit', 2336, 0.5, FALSE),
    ('fighter_support_unit', 2337, 0.5, FALSE),
    ('fighter_support_unit', 2335, 0.5, FALSE),
    ('fighter_support_unit', 2338, 0.25, FALSE),
    ('fighter_support_unit', 50, 0.5, TRUE),
    ('fighter_support_unit', 30, 0.5, TRUE),
    -- Mining heads (crystal laser, plain laser, strip
    -- miner): yield rules, crit is the second income, waste
    -- rolls quietly tax every cycle.
    ('frequency_mining_laser', 77, 1.0, FALSE),
    ('frequency_mining_laser', 5967, 0.5, FALSE),
    ('frequency_mining_laser', 5969, 0.5, FALSE),
    ('frequency_mining_laser', 3154, 0.5, FALSE),
    ('frequency_mining_laser', 3153, 0.25, FALSE),
    ('frequency_mining_laser', 73, 0.25, FALSE),
    ('frequency_mining_laser', 54, 0.25, FALSE),
    ('frequency_mining_laser', 6, 0.25, FALSE),
    ('frequency_mining_laser', 50, 0.5, TRUE),
    ('frequency_mining_laser', 30, 0.5, TRUE),
    ('mining_laser', 77, 1.0, FALSE),
    ('mining_laser', 5967, 0.5, FALSE),
    ('mining_laser', 5969, 0.5, FALSE),
    ('mining_laser', 3154, 0.5, FALSE),
    ('mining_laser', 3153, 0.25, FALSE),
    ('mining_laser', 73, 0.25, FALSE),
    ('mining_laser', 54, 0.25, FALSE),
    ('mining_laser', 6, 0.25, FALSE),
    ('mining_laser', 50, 0.5, TRUE),
    ('mining_laser', 30, 0.5, TRUE),
    ('strip_miner', 77, 1.0, FALSE),
    ('strip_miner', 5967, 0.5, FALSE),
    ('strip_miner', 5969, 0.5, FALSE),
    ('strip_miner', 3154, 0.5, FALSE),
    ('strip_miner', 3153, 0.25, FALSE),
    ('strip_miner', 73, 0.25, FALSE),
    ('strip_miner', 54, 0.25, FALSE),
    ('strip_miner', 6, 0.25, FALSE),
    ('strip_miner', 50, 0.5, TRUE),
    ('strip_miner', 30, 0.5, TRUE),
    -- Gas harvesters and scoops: yield, then the waste tax.
    ('gas_cloud_harvesters', 77, 1.0, FALSE),
    ('gas_cloud_harvesters', 3154, 0.5, FALSE),
    ('gas_cloud_harvesters', 3153, 0.25, FALSE),
    ('gas_cloud_harvesters', 73, 0.25, FALSE),
    ('gas_cloud_harvesters', 54, 0.25, FALSE),
    ('gas_cloud_harvesters', 6, 0.25, FALSE),
    ('gas_cloud_harvesters', 50, 0.5, TRUE),
    ('gas_cloud_harvesters', 30, 0.5, TRUE),
    ('gas_cloud_scoops', 77, 1.0, FALSE),
    ('gas_cloud_scoops', 3154, 0.5, FALSE),
    ('gas_cloud_scoops', 3153, 0.25, FALSE),
    ('gas_cloud_scoops', 73, 0.25, FALSE),
    ('gas_cloud_scoops', 54, 0.25, FALSE),
    ('gas_cloud_scoops', 6, 0.25, FALSE),
    ('gas_cloud_scoops', 50, 0.5, TRUE),
    ('gas_cloud_scoops', 30, 0.5, TRUE),
    -- Mining drones: yield and the speed to reach the rock.
    ('mining_drone', 77, 1.0, FALSE),
    ('mining_drone', 37, 0.5, FALSE),
    ('mining_drone', 3154, 0.5, FALSE),
    ('mining_drone', 3153, 0.25, FALSE),
    ('mining_drone', 73, 0.25, FALSE),
    ('mining_drone', 54, 0.25, FALSE),
    -- Propulsion (afterburners and microwarpdrives share
    -- the group and the shape): velocity is the product,
    -- signature bloom is its price.
    ('propulsion_module', 20, 1.0, FALSE),
    ('propulsion_module', 554, 0.5, FALSE),
    ('propulsion_module', 6, 0.25, FALSE),
    ('propulsion_module', 50, 0.5, TRUE),
    ('propulsion_module', 30, 0.5, TRUE),
    -- Remote armor repairer: amount, then reach (optimal
    -- and falloff together), then the running costs.
    ('remote_armor_repairer', 84, 1.0, FALSE),
    ('remote_armor_repairer', 54, 0.5, FALSE),
    ('remote_armor_repairer', 2044, 0.5, FALSE),
    ('remote_armor_repairer', 73, 0.25, FALSE),
    ('remote_armor_repairer', 6, 0.25, FALSE),
    ('remote_armor_repairer', 50, 0.5, TRUE),
    ('remote_armor_repairer', 30, 0.5, TRUE),
    -- Shield booster: amount, then cycle time, then cap.
    ('shield_booster', 68, 1.0, FALSE),
    ('shield_booster', 73, 0.5, FALSE),
    ('shield_booster', 6, 0.25, FALSE),
    ('shield_booster', 50, 0.5, TRUE),
    ('shield_booster', 30, 0.5, TRUE),
    -- Shield extender: capacity is the product; signature
    -- bloom is its price.
    ('shield_extender', 72, 1.0, FALSE),
    ('shield_extender', 983, 0.5, FALSE),
    ('shield_extender', 50, 0.5, TRUE),
    ('shield_extender', 30, 0.5, TRUE),
    -- Siege module: both damage bonuses headline (a hull
    -- rolls the one it uses); local logistics trails.
    ('siege_module', 2306, 1.0, FALSE),
    ('siege_module', 2307, 1.0, FALSE),
    ('siege_module', 2347, 0.5, FALSE),
    ('siege_module', 2346, 0.25, FALSE),
    ('siege_module', 30, 0.5, TRUE),
    -- Smartbomb: damage (one flavor per bomb; all four are
    -- seeded, a roll scores only the one it has), then area.
    ('smart_bomb', 114, 1.0, FALSE),
    ('smart_bomb', 116, 1.0, FALSE),
    ('smart_bomb', 117, 1.0, FALSE),
    ('smart_bomb', 118, 1.0, FALSE),
    ('smart_bomb', 99, 0.5, FALSE),
    ('smart_bomb', 73, 0.25, FALSE),
    ('smart_bomb', 6, 0.25, FALSE),
    ('smart_bomb', 50, 0.5, TRUE),
    ('smart_bomb', 30, 0.5, TRUE),
    -- Stasis web: strength of the slow, then reach.
    ('stasis_web', 20, 1.0, FALSE),
    ('stasis_web', 54, 0.5, FALSE),
    ('stasis_web', 6, 0.25, FALSE),
    ('stasis_web', 50, 0.5, TRUE),
    -- Warp tackle (disruptors and scramblers, group 52):
    -- range is the whole game at this class; CPU is the
    -- classic brick-maker; cap use trails.
    ('warp_scrambler', 54, 1.0, FALSE),
    ('warp_scrambler', 6, 0.25, FALSE),
    ('warp_scrambler', 50, 0.5, TRUE)
ON CONFLICT (family, attribute_id) DO NOTHING;
