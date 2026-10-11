-- Step 032: corp pulse change log. pulse_seen holds the
-- member and structure IDs last noticed per corporation;
-- pulse_events records departures and structure arrivals /
-- removals as the worker notices them. The widget shows recent
-- events; old ones are pruned. No ESI traffic: the worker diffs
-- already-stored corp snapshots.
CREATE TABLE IF NOT EXISTS pulse_seen (
  corp_id BIGINT NOT NULL,
  kind TEXT NOT NULL,
  key TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (corp_id, kind, key)
);
CREATE TABLE IF NOT EXISTS pulse_events (
  corp_id BIGINT NOT NULL,
  kind TEXT NOT NULL,
  key TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (corp_id, kind, key, at)
);
CREATE INDEX IF NOT EXISTS pulse_events_recent ON pulse_events (corp_id, at DESC);
