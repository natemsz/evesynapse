-- Sov fights (sov_fights.go): every sovereignty contest the campaigns
-- mirror has seen, kept after ESI drops it, and the kills fought in
-- it. A kill belongs to a fight when the defending alliance took part
-- in it, in the contested constellation, from an hour before the timer
-- until the contest ends. side says how: 1 the defender lost the ship,
-- 2 the defender was on the attacker list.
CREATE TABLE IF NOT EXISTS sov_fights (
    campaign_id         BIGINT PRIMARY KEY,
    system_id           BIGINT NOT NULL,
    constellation_id    BIGINT NOT NULL DEFAULT 0,
    region_id           BIGINT NOT NULL DEFAULT 0,
    structure_id        BIGINT NOT NULL DEFAULT 0,
    event_type          TEXT NOT NULL DEFAULT '',
    defender_id         BIGINT NOT NULL DEFAULT 0,
    start_time          TIMESTAMPTZ NOT NULL,
    first_seen          TIMESTAMPTZ NOT NULL,
    last_seen           TIMESTAMPTZ NOT NULL,
    ended_at            TIMESTAMPTZ,
    defender_score      DOUBLE PRECISION NOT NULL DEFAULT 0,
    attackers_score     DOUBLE PRECISION NOT NULL DEFAULT 0,
    kills               BIGINT NOT NULL DEFAULT 0,
    isk                 DOUBLE PRECISION NOT NULL DEFAULT 0,
    defender_lost       BIGINT NOT NULL DEFAULT 0,
    defender_lost_isk   DOUBLE PRECISION NOT NULL DEFAULT 0,
    defender_killed     BIGINT NOT NULL DEFAULT 0,
    defender_killed_isk DOUBLE PRECISION NOT NULL DEFAULT 0,
    last_kill_at        TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS sov_fights_open_idx ON sov_fights (ended_at);
CREATE INDEX IF NOT EXISTS sov_fights_ended_idx ON sov_fights (ended_at DESC) WHERE ended_at IS NOT NULL;

CREATE TABLE IF NOT EXISTS sov_fight_kills (
    campaign_id   BIGINT NOT NULL,
    killmail_id   BIGINT NOT NULL,
    killmail_time TIMESTAMPTZ NOT NULL,
    system_id     BIGINT NOT NULL,
    side          SMALLINT NOT NULL DEFAULT 0,
    PRIMARY KEY (campaign_id, killmail_id)
);
CREATE INDEX IF NOT EXISTS sov_fight_kills_page_idx ON sov_fight_kills (campaign_id, killmail_time DESC, killmail_id DESC);
CREATE INDEX IF NOT EXISTS sov_fight_kills_time_idx ON sov_fight_kills (killmail_time DESC, killmail_id DESC);
