-- Schema step 163: who runs an owner's programmes.
--
-- owner_permissions is the choice an owner makes of who, besides the
-- people who run its programmes by position, may run one of them. One
-- choice per owner and programme, in the terms of a Discord role rule.
-- It starts with every corporation's stored choice.
--
-- While corp_permissions remains, a trigger carries each change made
-- to it into owner_permissions in the same transaction, so a choice
-- made under the previous release holds when this one returns.

CREATE TABLE IF NOT EXISTS owner_permissions (
    owner_kind TEXT NOT NULL CHECK (owner_kind IN ('corporation', 'alliance', 'community')),
    owner_id   BIGINT NOT NULL,
    permission TEXT NOT NULL,
    kind       TEXT NOT NULL,
    ref        TEXT NOT NULL DEFAULT '',
    updated_by BIGINT NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_kind, owner_id, permission)
);

INSERT INTO owner_permissions (owner_kind, owner_id, permission, kind, ref, updated_by, updated_at)
SELECT 'corporation', corporation_id, permission, kind, ref, updated_by, updated_at
FROM corp_permissions
ON CONFLICT (owner_kind, owner_id, permission) DO NOTHING;

CREATE OR REPLACE FUNCTION corp_permissions_to_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM owner_permissions
        WHERE owner_kind = 'corporation' AND owner_id = OLD.corporation_id AND permission = OLD.permission;
        RETURN OLD;
    END IF;
    INSERT INTO owner_permissions (owner_kind, owner_id, permission, kind, ref, updated_by, updated_at)
    VALUES ('corporation', NEW.corporation_id, NEW.permission, NEW.kind, NEW.ref, NEW.updated_by, NEW.updated_at)
    ON CONFLICT (owner_kind, owner_id, permission) DO UPDATE SET
        kind       = excluded.kind,
        ref        = excluded.ref,
        updated_by = excluded.updated_by,
        updated_at = excluded.updated_at;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS corp_permissions_to_owner ON corp_permissions;
CREATE TRIGGER corp_permissions_to_owner
AFTER INSERT OR UPDATE OR DELETE ON corp_permissions
FOR EACH ROW EXECUTE FUNCTION corp_permissions_to_owner();
