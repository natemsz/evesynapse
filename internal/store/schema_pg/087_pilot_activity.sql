-- Pilot activity profiles (schema 087): what the public kill
-- record says about one pilot, kept as stored aggregates
-- (internal/app/pilot_activity.go). A pilot first met in an intel
-- reading is backfilled once from zKillboard's history; after
-- that the live kill stream keeps the profile current. Pages
-- read these rows only, never the raw kills.
CREATE TABLE IF NOT EXISTS pilot_activity_profiles (
    character_id       BIGINT PRIMARY KEY,
    backfilled_at      TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL,
    kills              BIGINT NOT NULL DEFAULT 0,
    losses             BIGINT NOT NULL DEFAULT 0,
    final_blows        BIGINT NOT NULL DEFAULT 0,
    gang_total         BIGINT NOT NULL DEFAULT 0,
    gang_samples       BIGINT NOT NULL DEFAULT 0,
    usual_ship_type_id BIGINT NOT NULL DEFAULT 0,
    last_active_at     TIMESTAMPTZ
);

-- The ships of a pilot's record: flown as an attacker, final
-- blows in them, lost as a victim.
CREATE TABLE IF NOT EXISTS pilot_activity_ships (
    character_id BIGINT NOT NULL,
    ship_type_id BIGINT NOT NULL,
    flown        BIGINT NOT NULL DEFAULT 0,
    final_blows  BIGINT NOT NULL DEFAULT 0,
    lost         BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (character_id, ship_type_id)
);

-- Where a pilot's record happened, and when last.
CREATE TABLE IF NOT EXISTS pilot_activity_systems (
    character_id BIGINT NOT NULL,
    system_id    BIGINT NOT NULL,
    events       BIGINT NOT NULL DEFAULT 0,
    last_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (character_id, system_id)
);

-- The hours (EVE time, UTC) of a pilot's record.
CREATE TABLE IF NOT EXISTS pilot_activity_hours (
    character_id BIGINT NOT NULL,
    hour         SMALLINT NOT NULL,
    events       BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (character_id, hour)
);

-- One marker per (kill, pilot) already folded into the
-- aggregates, with the kill's time so old markers prune on
-- their own schedule: the history backfill and the live stream
-- can never count the same kill twice for the same pilot.
-- Pruned with the kill details after a year.
CREATE TABLE IF NOT EXISTS pilot_activity_applied (
    killmail_id   BIGINT NOT NULL,
    character_id  BIGINT NOT NULL,
    killmail_time TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (killmail_id, character_id)
);
CREATE INDEX IF NOT EXISTS pilot_activity_applied_time_idx ON pilot_activity_applied (killmail_time);
