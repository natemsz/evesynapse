-- Schema step 167: the live fleet.
--
-- While a fleet runs, the worker reads its members from whichever
-- commander is the boss and stores what it saw. Pages read only these
-- rows and never call ESI.

-- Where each pilot was when last seen in the fleet. fleet_position is
-- ESI's place in the command chain; suggested_role is the part their
-- hull plays (DPS, Logistics and so on).
ALTER TABLE op_attendance
    ADD COLUMN IF NOT EXISTS wing_id         BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS squad_id        BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS fleet_position  TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS solar_system_id BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS suggested_role  TEXT NOT NULL DEFAULT '';

-- Which commander the fleet was last read through, and how each
-- commander's read went (JSON), so the page can say why in plain words.
ALTER TABLE ops
    ADD COLUMN IF NOT EXISTS capture_boss_id BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS capture_detail  TEXT NOT NULL DEFAULT '';

-- Finding the fleets to read stays a short index range however many
-- past fleets are stored.
CREATE INDEX IF NOT EXISTS ops_capture_window ON ops (starts_at, id)
    WHERE cancelled_at IS NULL AND fc_character_id <> 0;

-- What a commander assigned to a pilot on this fleet: at most one role,
-- and a few short tags.
CREATE TABLE IF NOT EXISTS op_member_tags (
    op_id        BIGINT NOT NULL REFERENCES ops (id) ON DELETE CASCADE,
    character_id BIGINT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'tag' CHECK (kind IN ('role', 'tag')),
    tag          TEXT NOT NULL CHECK (char_length(tag) BETWEEN 1 AND 24),
    set_by       BIGINT NOT NULL DEFAULT 0,
    set_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (op_id, character_id, kind, tag)
);
CREATE UNIQUE INDEX IF NOT EXISTS op_member_one_role
    ON op_member_tags (op_id, character_id) WHERE kind = 'role';
CREATE INDEX IF NOT EXISTS op_member_tags_character ON op_member_tags (character_id);

-- The fleet as last read, ready to render. revision moves on every
-- write, so a polling page can tell when there is something new.
CREATE TABLE IF NOT EXISTS op_composition (
    op_id       BIGINT PRIMARY KEY REFERENCES ops (id) ON DELETE CASCADE,
    payload     TEXT NOT NULL,
    revision    BIGINT NOT NULL DEFAULT 1,
    computed_at timestamptz NOT NULL
);

-- Ending a character's identity also clears what commanders noted
-- about it.
CREATE OR REPLACE FUNCTION release_community_character(pilot BIGINT) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    led   BIGINT[];
    owned BIGINT;
    heir  BIGINT;
BEGIN
    SELECT COALESCE(array_agg(community_id), '{}') INTO led
    FROM community_members
    WHERE character_id = pilot AND role = 'leader';

    DELETE FROM community_members WHERE character_id = pilot AND state <> 'banned';

    FOREACH owned IN ARRAY led LOOP
        SELECT m.character_id INTO heir
        FROM community_members m
        JOIN characters c ON c.character_id = m.character_id AND c.link_state = 'ok'
        JOIN verified_pilots v ON v.character_id = m.character_id
        WHERE m.community_id = owned AND m.role = 'admin' AND m.state = 'active'
          AND (v.owner_hash = '' OR c.owner_hash = '' OR v.owner_hash = c.owner_hash)
        ORDER BY m.joined_at, m.character_id
        LIMIT 1;
        IF FOUND THEN
            UPDATE community_members SET role = 'leader'
            WHERE community_id = owned AND character_id = heir;
        ELSE
            UPDATE communities SET archived_at = now()
            WHERE id = owned AND archived_at IS NULL;
        END IF;
    END LOOP;

    DELETE FROM op_signups WHERE character_id = pilot AND user_id IS NULL;
    DELETE FROM op_commanders WHERE character_id = pilot;
    DELETE FROM op_member_tags WHERE character_id = pilot;
    DELETE FROM verified_pilots WHERE character_id = pilot;
END;
$$;
