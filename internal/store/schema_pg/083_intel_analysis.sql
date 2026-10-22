-- What each intel report says (schema 083, internal/intelanalyze):
-- its kind (a sighting, a camp, a fleet, capitals, a bubble, someone
-- in Local, friendlies, a clear, a question, chatter) and the pilots,
-- ships, gate and flags read from it, as JSON. analysis_pending marks
-- a report with names still to look up; the worker's short loop looks
-- them up and analyses it again (intel_analysis.go).
ALTER TABLE intel_reports ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';
ALTER TABLE intel_reports ADD COLUMN IF NOT EXISTS analysis TEXT NOT NULL DEFAULT '';
ALTER TABLE intel_reports ADD COLUMN IF NOT EXISTS analysis_pending BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS intel_reports_pending_idx ON intel_reports (created_at) WHERE analysis_pending;

-- Names read from reports, looked up with EVE: character_id 0 is
-- "no such pilot", so the same words are not asked about again.
CREATE TABLE IF NOT EXISTS intel_pilot_names (
    name_lower   TEXT PRIMARY KEY,
    character_id BIGINT NOT NULL DEFAULT 0,
    name         TEXT NOT NULL DEFAULT '',
    checked_at   TIMESTAMPTZ NOT NULL
);

-- Each named pilot's corporation and alliance, for telling friend
-- from foe.
CREATE TABLE IF NOT EXISTS intel_pilot_affiliations (
    character_id   BIGINT PRIMARY KEY,
    corporation_id BIGINT NOT NULL DEFAULT 0,
    alliance_id    BIGINT NOT NULL DEFAULT 0,
    checked_at     TIMESTAMPTZ NOT NULL
);
