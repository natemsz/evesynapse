-- Intel channels owned by a corporation or alliance (schema 082), and
-- shared by the same rules as Discord roles and the corporation's
-- programmes (internal/standing): members, a corporation of the
-- alliance, CEOs, an in-game role, a group, everyone connected.
--
-- owner_kind '' is a personal channel (owner_user_id manages it, as
-- before). 'corporation' or 'alliance' with owner_id is the
-- organisation's: its directors manage it, and for a corporation
-- whoever they hand "intel channels" to (corp_permissions).
-- owner_user_id then only records who made it.
ALTER TABLE intel_channels ADD COLUMN IF NOT EXISTS owner_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE intel_channels ADD COLUMN IF NOT EXISTS owner_id BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS intel_channels_org_idx ON intel_channels (owner_kind, owner_id) WHERE owner_kind <> '';

-- A rule grant: everyone who meets the rule, judged against the
-- channel's owner organisation, reads the channel (and posts, with
-- can_post). Rule kinds are internal/standing's, never renamed.
CREATE TABLE IF NOT EXISTS intel_channel_rules (
    channel_id BIGINT NOT NULL REFERENCES intel_channels (id) ON DELETE CASCADE,
    rule_kind  TEXT NOT NULL,
    rule_ref   TEXT NOT NULL DEFAULT '',
    can_post   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (channel_id, rule_kind, rule_ref)
);
