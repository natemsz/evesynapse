-- The killboard's leaderboards (killboard_leaders.go). The worker
-- ranks each period's kills (the last 24 hours, 7 days and 30 days)
-- per scope and stores the leaders; the pages read these rows only.
-- Each rank adds up stored totals (kill_leaders_totals, below).
--
-- scope_kind '' is New Eden; 'region' and 'system' rank kills in a
-- place; 'ship' ranks the attackers flying a hull; 'alliance' and
-- 'corporation' rank their own members. board names what is ranked:
-- 'character', 'corporation' and 'alliance' (attackers), 'ship_lost'
-- (victim hulls), 'ship_used' (attacker hulls), 'system', 'region',
-- and 'kill' (single kills by value, entity_id the killmail id).
-- Each row carries its rank by kills and by ISK; a row is kept when
-- either rank makes the scope's cut.
CREATE TABLE IF NOT EXISTS kill_leaders (
    period     TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id   BIGINT NOT NULL,
    board      TEXT NOT NULL,
    entity_id  BIGINT NOT NULL,
    kills      BIGINT NOT NULL DEFAULT 0,
    solo       BIGINT NOT NULL DEFAULT 0,
    isk        DOUBLE PRECISION NOT NULL DEFAULT 0,
    rank_kills INTEGER NOT NULL,
    rank_isk   INTEGER NOT NULL,
    PRIMARY KEY (period, scope_kind, scope_id, board, entity_id)
);

-- When each period was last ranked, over which window.
CREATE TABLE IF NOT EXISTS kill_leaders_state (
    period      TEXT PRIMARY KEY,
    since       TIMESTAMPTZ NOT NULL,
    computed_at TIMESTAMPTZ NOT NULL,
    kills       BIGINT NOT NULL DEFAULT 0
);

-- The ranking's working set: the period's kills and their attackers,
-- refilled on every run. Unlogged: nothing here outlives a run.
CREATE UNLOGGED TABLE IF NOT EXISTS kill_leaders_win (
    killmail_id  BIGINT PRIMARY KEY,
    system_id    BIGINT NOT NULL,
    region_id    BIGINT NOT NULL,
    ship_type_id BIGINT NOT NULL,
    value        DOUBLE PRECISION NOT NULL,
    solo         BOOLEAN NOT NULL,
    v_character  BIGINT NOT NULL,
    v_corporation BIGINT NOT NULL,
    v_alliance   BIGINT NOT NULL
);
CREATE UNLOGGED TABLE IF NOT EXISTS kill_leaders_att (
    killmail_id    BIGINT NOT NULL,
    character_id   BIGINT NOT NULL,
    corporation_id BIGINT NOT NULL,
    alliance_id    BIGINT NOT NULL,
    ship_type_id   BIGINT NOT NULL
);

-- The totals the ranks add up, uncut, by EVE day; the rolling last
-- 24 hours sit under day 1970-01-01. The 7 and 30 day ranks add up
-- their days, so a run reads each past day's totals instead of every
-- kill again. The single-kill board keeps each scope's biggest only,
-- which is all a sum of days can rank. A finished day is totalled
-- once, and again a few hours later for late killmails; today, before
-- each rank that needs it. Kept small, as there are many: board is
-- 1 character, 2 corporation, 3 alliance, 4 ship_lost, 5 ship_used,
-- 6 system, 7 region, 8 kill; scope is 0 New Eden, 1 region,
-- 2 system, 3 ship, 4 alliance, 5 corporation.
CREATE TABLE IF NOT EXISTS kill_leaders_totals (
    day       DATE NOT NULL,
    board     SMALLINT NOT NULL,
    scope     SMALLINT NOT NULL,
    scope_id  BIGINT NOT NULL,
    entity_id BIGINT NOT NULL,
    kills     INTEGER NOT NULL DEFAULT 0,
    solo      INTEGER NOT NULL DEFAULT 0,
    isk       DOUBLE PRECISION NOT NULL DEFAULT 0,
    PRIMARY KEY (day, board, scope, scope_id, entity_id)
);
CREATE TABLE IF NOT EXISTS kill_leaders_days (
    day         DATE PRIMARY KEY,
    computed_at TIMESTAMPTZ NOT NULL
);
