-- The sovereignty tracker (intel_sov.go). Each system's holder row
-- gains the claim detail GET /sovereignty/systems/ carries: when
-- the holder took it, the sovereignty hub's vulnerability window,
-- the capital flag and the development levels (ADM among them).
ALTER TABLE system_sovereignty
    ADD COLUMN IF NOT EXISTS claimed_since    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS hub_id           BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS vuln_start       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS vuln_end         TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS is_capital       BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS adm              DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS military_level   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS industrial_level INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS strategic_level  INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS system_sovereignty_alliance_idx ON system_sovereignty (alliance_id) WHERE alliance_id > 0;
CREATE INDEX IF NOT EXISTS system_sovereignty_claimed_idx ON system_sovereignty (claimed_since DESC) WHERE alliance_id > 0;
CREATE INDEX IF NOT EXISTS system_sovereignty_vuln_idx ON system_sovereignty (vuln_end) WHERE alliance_id > 0;

-- Every change of holder the sovereignty pass sees, written when it
-- replaces the holder set: who held the system before and who holds
-- it now. ESI keeps no history, so the log starts with the first
-- pass after this step.
CREATE TABLE IF NOT EXISTS sov_changes (
    id                 BIGSERIAL PRIMARY KEY,
    system_id          BIGINT NOT NULL,
    changed_at         TIMESTAMPTZ NOT NULL,
    old_alliance_id    BIGINT NOT NULL DEFAULT 0,
    old_corporation_id BIGINT NOT NULL DEFAULT 0,
    old_faction_id     BIGINT NOT NULL DEFAULT 0,
    new_alliance_id    BIGINT NOT NULL DEFAULT 0,
    new_corporation_id BIGINT NOT NULL DEFAULT 0,
    new_faction_id     BIGINT NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS sov_changes_at_idx ON sov_changes (changed_at DESC);
CREATE INDEX IF NOT EXISTS sov_changes_old_alliance_idx ON sov_changes (old_alliance_id, changed_at DESC) WHERE old_alliance_id > 0;
CREATE INDEX IF NOT EXISTS sov_changes_new_alliance_idx ON sov_changes (new_alliance_id, changed_at DESC) WHERE new_alliance_id > 0;
CREATE INDEX IF NOT EXISTS sov_changes_system_idx ON sov_changes (system_id, changed_at DESC);

-- The live sovereignty contests (GET /sovereignty/campaigns/),
-- replaced whole on each fetch.
CREATE TABLE IF NOT EXISTS sov_campaigns (
    campaign_id      BIGINT PRIMARY KEY,
    system_id        BIGINT NOT NULL,
    constellation_id BIGINT NOT NULL DEFAULT 0,
    structure_id     BIGINT NOT NULL DEFAULT 0,
    event_type       TEXT NOT NULL DEFAULT '',
    defender_id      BIGINT NOT NULL DEFAULT 0,
    defender_score   DOUBLE PRECISION NOT NULL DEFAULT 0,
    attackers_score  DOUBLE PRECISION NOT NULL DEFAULT 0,
    start_time       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sov_campaigns_system_idx ON sov_campaigns (system_id);
