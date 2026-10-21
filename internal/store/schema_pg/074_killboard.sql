-- The killboard (schema 074): every killmail in New Eden.
--
-- Kills arrive two ways. zKillboard's public R2Z2 stream carries
-- every posted killmail (killfeed.go), and the characters signed in
-- here carry their own through ESI (killmail_details). A kill is
-- stored once whichever comes first. One the stream carried is
-- public; one only a character's token reached is not, until the
-- stream carries it too: it still counts in aggregates (activity,
-- threat), but no page shows it, because nobody posted it.
--
-- kills is the summary and is kept forever: what died, where, when,
-- whose it was, who landed the final blow, and what it was worth --
-- priced by our own appraisal (value_ours, with value_coverage the
-- share of the appraisal's lines it could price), zKillboard's
-- figure kept beside it. kill_attackers and kill_details (the full
-- ESI killmail, gzipped) are kept for a year and then pruned.
CREATE TABLE IF NOT EXISTS kills (
    killmail_id            BIGINT PRIMARY KEY,
    hash                   TEXT NOT NULL DEFAULT '',
    killmail_time          TIMESTAMPTZ NOT NULL,
    system_id              BIGINT NOT NULL,
    region_id              BIGINT NOT NULL DEFAULT 0,
    location_id            BIGINT NOT NULL DEFAULT 0,
    victim_character_id    BIGINT NOT NULL DEFAULT 0,
    victim_corporation_id  BIGINT NOT NULL DEFAULT 0,
    victim_alliance_id     BIGINT NOT NULL DEFAULT 0,
    victim_faction_id      BIGINT NOT NULL DEFAULT 0,
    ship_type_id           BIGINT NOT NULL DEFAULT 0,
    damage_taken           BIGINT NOT NULL DEFAULT 0,
    attacker_count         INTEGER NOT NULL DEFAULT 0,
    fb_character_id        BIGINT NOT NULL DEFAULT 0,
    fb_corporation_id      BIGINT NOT NULL DEFAULT 0,
    fb_alliance_id         BIGINT NOT NULL DEFAULT 0,
    fb_ship_type_id        BIGINT NOT NULL DEFAULT 0,
    npc                    BOOLEAN NOT NULL DEFAULT FALSE,
    solo                   BOOLEAN NOT NULL DEFAULT FALSE,
    awox                   BOOLEAN NOT NULL DEFAULT FALSE,
    points                 INTEGER NOT NULL DEFAULT 0,
    value_zkill            DOUBLE PRECISION,
    value_ours             DOUBLE PRECISION,
    value_dropped_ours     DOUBLE PRECISION,
    value_coverage         REAL,
    priced_at              TIMESTAMPTZ,
    source                 TEXT NOT NULL,
    public                 BOOLEAN NOT NULL DEFAULT TRUE,
    ingested_at            TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS kills_time_idx ON kills (killmail_time DESC);
CREATE INDEX IF NOT EXISTS kills_system_idx ON kills (system_id, killmail_time DESC);
CREATE INDEX IF NOT EXISTS kills_region_idx ON kills (region_id, killmail_time DESC);
CREATE INDEX IF NOT EXISTS kills_victim_char_idx ON kills (victim_character_id, killmail_time DESC) WHERE victim_character_id > 0;
CREATE INDEX IF NOT EXISTS kills_victim_corp_idx ON kills (victim_corporation_id, killmail_time DESC) WHERE victim_corporation_id > 0;
CREATE INDEX IF NOT EXISTS kills_victim_alliance_idx ON kills (victim_alliance_id, killmail_time DESC) WHERE victim_alliance_id > 0;
CREATE INDEX IF NOT EXISTS kills_ship_idx ON kills (ship_type_id, killmail_time DESC);
CREATE INDEX IF NOT EXISTS kills_unpriced_idx ON kills (killmail_time DESC) WHERE priced_at IS NULL;

CREATE TABLE IF NOT EXISTS kill_attackers (
    killmail_id     BIGINT NOT NULL,
    seq             INTEGER NOT NULL,
    character_id    BIGINT NOT NULL DEFAULT 0,
    corporation_id  BIGINT NOT NULL DEFAULT 0,
    alliance_id     BIGINT NOT NULL DEFAULT 0,
    faction_id      BIGINT NOT NULL DEFAULT 0,
    ship_type_id    BIGINT NOT NULL DEFAULT 0,
    weapon_type_id  BIGINT NOT NULL DEFAULT 0,
    damage_done     BIGINT NOT NULL DEFAULT 0,
    final_blow      BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (killmail_id, seq)
);
CREATE INDEX IF NOT EXISTS kill_attackers_char_idx ON kill_attackers (character_id, killmail_id DESC) WHERE character_id > 0;
CREATE INDEX IF NOT EXISTS kill_attackers_corp_idx ON kill_attackers (corporation_id, killmail_id DESC) WHERE corporation_id > 0;
CREATE INDEX IF NOT EXISTS kill_attackers_alliance_idx ON kill_attackers (alliance_id, killmail_id DESC) WHERE alliance_id > 0;

CREATE TABLE IF NOT EXISTS kill_details (
    killmail_id BIGINT PRIMARY KEY,
    payload     BYTEA NOT NULL,
    stored_at   TIMESTAMPTZ NOT NULL
);

-- The stream follower's cursor: the next R2Z2 sequence to read.
CREATE TABLE IF NOT EXISTS killfeed_state (
    id         INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    sequence   BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
