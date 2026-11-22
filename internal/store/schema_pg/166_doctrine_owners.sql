-- Schema step 166: doctrines with owners, and what a fleet flies.
--
-- A doctrine belongs to a corporation, alliance or community. Its
-- members read it, and it can be opened to anyone with its link.
--
-- corporation_id stays for the previous release, whose insert names
-- the (corporation_id, lower(name)) index and whose name sweep reads
-- the column as a number. A doctrine of any other owner stores minus
-- its own id there: unique, never a real corporation, and skipped by
-- that sweep.

ALTER TABLE doctrines
    ADD COLUMN IF NOT EXISTS owner_kind TEXT NOT NULL DEFAULT 'corporation',
    ADD COLUMN IF NOT EXISTS owner_id   BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'members';

UPDATE doctrines SET owner_id = corporation_id
WHERE owner_kind = 'corporation' AND owner_id <> corporation_id;

CREATE OR REPLACE FUNCTION doctrines_owner_columns() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.owner_kind = 'corporation' THEN
        NEW.owner_id := NEW.corporation_id;
    ELSE
        NEW.corporation_id := -NEW.id;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS doctrines_owner_columns ON doctrines;
CREATE TRIGGER doctrines_owner_columns
BEFORE INSERT OR UPDATE OF owner_kind, owner_id, corporation_id ON doctrines
FOR EACH ROW EXECUTE FUNCTION doctrines_owner_columns();

ALTER TABLE doctrines DROP CONSTRAINT IF EXISTS doctrines_owner_kind_check;
ALTER TABLE doctrines ADD CONSTRAINT doctrines_owner_kind_check
    CHECK (owner_kind IN ('corporation', 'alliance', 'community'));
ALTER TABLE doctrines DROP CONSTRAINT IF EXISTS doctrines_owner_matches_corporation;
ALTER TABLE doctrines ADD CONSTRAINT doctrines_owner_matches_corporation
    CHECK ((owner_kind = 'corporation' AND owner_id = corporation_id)
        OR (owner_kind <> 'corporation' AND owner_id > 0 AND corporation_id = -id));
ALTER TABLE doctrines DROP CONSTRAINT IF EXISTS doctrines_visibility_check;
ALTER TABLE doctrines ADD CONSTRAINT doctrines_visibility_check
    CHECK (visibility IN ('members', 'public'));

CREATE UNIQUE INDEX IF NOT EXISTS doctrines_owner_name ON doctrines (owner_kind, owner_id, lower(name));

-- What a fleet flies: nothing in particular, a doctrine it suggests, or
-- one it requires. The snapshot is the doctrine as it stood when the
-- fleet started, so a later edit cannot change what the fleet flew.
ALTER TABLE ops
    ADD COLUMN IF NOT EXISTS doctrine_mode           TEXT NOT NULL DEFAULT 'none',
    ADD COLUMN IF NOT EXISTS doctrine_snapshot       TEXT,
    ADD COLUMN IF NOT EXISTS doctrine_snapshot_ships BIGINT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS doctrine_snapshot_at    timestamptz;

UPDATE ops SET doctrine_mode = 'suggested'
WHERE doctrine_mode = 'none' AND (doctrine_id <> 0 OR doctrine <> '');

ALTER TABLE ops DROP CONSTRAINT IF EXISTS ops_doctrine_mode_check;
ALTER TABLE ops ADD CONSTRAINT ops_doctrine_mode_check
    CHECK (doctrine_mode IN ('none', 'suggested', 'required'));

-- A fleet that names a doctrine is never 'none', whichever release
-- wrote it. Choosing another doctrine, or moving the start back into
-- the future, drops the snapshot so the new start takes a fresh one.
CREATE OR REPLACE FUNCTION ops_doctrine_state() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.doctrine_id = 0 AND NEW.doctrine = '' THEN
        NEW.doctrine_mode := 'none';
    ELSIF NEW.doctrine_mode = 'none' THEN
        NEW.doctrine_mode := 'suggested';
    END IF;
    IF TG_OP = 'UPDATE' AND (NEW.doctrine_id <> OLD.doctrine_id OR NEW.starts_at > now()) THEN
        NEW.doctrine_snapshot := NULL;
        NEW.doctrine_snapshot_ships := '{}';
        NEW.doctrine_snapshot_at := NULL;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS ops_doctrine_state ON ops;
CREATE TRIGGER ops_doctrine_state
BEFORE INSERT OR UPDATE OF doctrine, doctrine_id, doctrine_mode, starts_at ON ops
FOR EACH ROW EXECUTE FUNCTION ops_doctrine_state();

CREATE INDEX IF NOT EXISTS ops_doctrine_snapshot_due ON ops (starts_at, id)
    WHERE doctrine_id <> 0 AND doctrine_snapshot_at IS NULL;
