-- Kill involvement (schema 127): the permanent record of who was
-- on a kill. One slim row per attacker per kill, carrying only
-- the identity columns: the killmail, and the attacker's
-- character, corporation and alliance.
--
-- kill_attackers carries the detail of a kill (ships, weapons,
-- damage, final blow) and ages out at the retention cutoff; the
-- summary row and the priced lines are kept forever. Involvement
-- never ages out, so a kill we have priced keeps crediting its
-- value to the pilots, corporations and alliances on it after
-- the detail is gone. The entity kill stats, the biggest-kill
-- lookups and the attacker sides of the board listings read
-- involvement; everything that needs attacker detail keeps
-- reading kill_attackers.
CREATE TABLE IF NOT EXISTS kill_involvement (
    killmail_id    BIGINT NOT NULL,
    character_id   BIGINT NOT NULL DEFAULT 0,
    corporation_id BIGINT NOT NULL DEFAULT 0,
    alliance_id    BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (killmail_id, character_id, corporation_id, alliance_id)
);
CREATE INDEX IF NOT EXISTS kill_involvement_char_idx ON kill_involvement (character_id, killmail_id DESC) WHERE character_id > 0;
CREATE INDEX IF NOT EXISTS kill_involvement_corp_idx ON kill_involvement (corporation_id, killmail_id DESC) WHERE corporation_id > 0;
CREATE INDEX IF NOT EXISTS kill_involvement_alliance_idx ON kill_involvement (alliance_id, killmail_id DESC) WHERE alliance_id > 0;

-- Every attacker row still on disk keeps its attribution: the
-- involvement record starts from the attackers we still hold.
INSERT INTO kill_involvement (killmail_id, character_id, corporation_id, alliance_id)
SELECT killmail_id, character_id, corporation_id, alliance_id FROM kill_attackers
ON CONFLICT DO NOTHING;
