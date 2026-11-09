-- Kill entity feed kill index (schema 134): the maintenance
-- sweep that backfills feed rows for kills stored before the
-- feed existed finds its work with an anti-join on killmail_id
-- (ListKillsMissingFeed). The feed's primary key leads with
-- (kind, entity_id), so that probe could not use it: the sweep
-- ran a nested-loop anti-join over the whole kills table,
-- burned two workers for the full statement timeout, and
-- errored every minute without ever draining. This index makes
-- the probe an index-only lookup per kill, like the applied
-- markers' primary key already does for the stats sweep.

CREATE INDEX IF NOT EXISTS kill_entity_feed_kill_idx
    ON kill_entity_feed (killmail_id);
