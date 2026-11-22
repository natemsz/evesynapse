-- Schema step 165: fleets.
--
-- An op becomes a fleet with an owner: a corporation, alliance or
-- community. corporation_id stays for the corporation features that
-- still read it, and is 0 on a fleet owned by anything else, so ship
-- replacement and the other corporation paths never pick one up.
--
-- Every existing op stays exactly as it was: restricted to its
-- corporation's members, who are also the ones who can sign up.

ALTER TABLE ops
    ADD COLUMN IF NOT EXISTS owner_kind    TEXT NOT NULL DEFAULT 'corporation',
    ADD COLUMN IF NOT EXISTS owner_id      BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS fleet_type    TEXT NOT NULL DEFAULT 'corporation',
    ADD COLUMN IF NOT EXISTS visibility    TEXT NOT NULL DEFAULT 'restricted',
    ADD COLUMN IF NOT EXISTS join_gate     TEXT NOT NULL DEFAULT 'rule',
    ADD COLUMN IF NOT EXISTS share_token   TEXT,
    ADD COLUMN IF NOT EXISTS roster_public BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS coming_count  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS maybe_count   INTEGER NOT NULL DEFAULT 0;

UPDATE ops SET owner_id = corporation_id WHERE owner_kind = 'corporation' AND owner_id <> corporation_id;

-- A row written without an owner, as the previous release writes them,
-- belongs to its corporation.
CREATE OR REPLACE FUNCTION ops_owner_from_corporation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_kind = 'corporation' THEN
        NEW.owner_id := NEW.corporation_id;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS ops_owner_from_corporation ON ops;
CREATE TRIGGER ops_owner_from_corporation
BEFORE INSERT OR UPDATE OF owner_kind, owner_id, corporation_id ON ops
FOR EACH ROW EXECUTE FUNCTION ops_owner_from_corporation();

ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_owner_kind_check;
ALTER TABLE ops ADD CONSTRAINT ops_owner_kind_check
    CHECK (owner_kind IN ('corporation', 'alliance', 'community'));
ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_owner_matches_corporation;
ALTER TABLE ops ADD CONSTRAINT ops_owner_matches_corporation
    CHECK (owner_kind = 'corporation' OR (corporation_id = 0 AND owner_id <> 0));
ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_fleet_type_check;
ALTER TABLE ops ADD CONSTRAINT ops_fleet_type_check
    CHECK (fleet_type IN ('npsi', 'corporation', 'alliance', 'community', 'custom'));
ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_visibility_check;
ALTER TABLE ops ADD CONSTRAINT ops_visibility_check
    CHECK (visibility IN ('public', 'unlisted', 'restricted'));
ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_join_gate_check;
ALTER TABLE ops ADD CONSTRAINT ops_join_gate_check
    CHECK (join_gate IN ('anyone', 'verified', 'account', 'rule'));

CREATE UNIQUE INDEX IF NOT EXISTS ops_share_token ON ops (share_token) WHERE share_token IS NOT NULL;
CREATE INDEX IF NOT EXISTS ops_owner_start ON ops (owner_kind, owner_id, starts_at);
CREATE INDEX IF NOT EXISTS ops_public_start ON ops (starts_at, id)
    WHERE visibility = 'public' AND cancelled_at IS NULL;
CREATE INDEX IF NOT EXISTS ops_fc_character ON ops (fc_character_id) WHERE fc_character_id <> 0;

-- A verified pilot signs up with no account.
ALTER TABLE op_signups ALTER COLUMN user_id DROP NOT NULL;
CREATE INDEX IF NOT EXISTS op_signups_character ON op_signups (character_id, op_id);

-- Who can see or join a fleet, when it is narrower than the owner's
-- members. No rows for an action means the owner's members.
CREATE TABLE IF NOT EXISTS op_access_rules (
    op_id     BIGINT NOT NULL REFERENCES ops (id) ON DELETE CASCADE,
    action    TEXT NOT NULL CHECK (action IN ('view', 'join')),
    rule_kind TEXT NOT NULL,
    rule_ref  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (op_id, action, rule_kind, rule_ref)
);

-- Commanders besides the fleet's main FC (ops.fc_character_id).
CREATE TABLE IF NOT EXISTS op_commanders (
    op_id        BIGINT NOT NULL REFERENCES ops (id) ON DELETE CASCADE,
    character_id BIGINT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'co_fc' CHECK (role IN ('co_fc')),
    added_by     BIGINT NOT NULL DEFAULT 0,
    added_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (op_id, character_id)
);
CREATE INDEX IF NOT EXISTS op_commanders_character ON op_commanders (character_id, op_id);

-- A fleet's sign-up counts are stored, so a list of fleets reads no
-- roster rows.
UPDATE ops o SET
    coming_count = (SELECT count(*) FROM op_signups s WHERE s.op_id = o.id AND s.response = 'yes'),
    maybe_count  = (SELECT count(*) FROM op_signups s WHERE s.op_id = o.id AND s.response = 'maybe');

CREATE OR REPLACE FUNCTION op_signups_counted() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    was_yes   BOOLEAN := TG_OP <> 'INSERT' AND OLD.response = 'yes';
    is_yes    BOOLEAN := TG_OP <> 'DELETE' AND NEW.response = 'yes';
    was_maybe BOOLEAN := TG_OP <> 'INSERT' AND OLD.response = 'maybe';
    is_maybe  BOOLEAN := TG_OP <> 'DELETE' AND NEW.response = 'maybe';
    target    BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        target := OLD.op_id;
    ELSE
        target := NEW.op_id;
    END IF;
    IF was_yes <> is_yes OR was_maybe <> is_maybe THEN
        UPDATE ops SET
            coming_count = coming_count + is_yes::int - was_yes::int,
            maybe_count  = maybe_count + is_maybe::int - was_maybe::int
        WHERE id = target;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS op_signups_counted ON op_signups;
CREATE TRIGGER op_signups_counted
AFTER INSERT OR UPDATE OR DELETE ON op_signups
FOR EACH ROW EXECUTE FUNCTION op_signups_counted();

-- Ending a character's identity also ends the sign-ups it made with no
-- account and any place it holds commanding a fleet.
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
    DELETE FROM verified_pilots WHERE character_id = pilot;
END;
$$;
