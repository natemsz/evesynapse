-- Corporation killboard coverage (schema 126): one marker row
-- per corporation whose kill history has been backfilled into
-- the kills store from zKillboard's history API, the corp twin
-- of the pilot activity profile's backfilled_at (schema 088).
-- Corporations keep no activity aggregates, so the marker is
-- all the backfill needs: a corporation is offered again only
-- while its marker is absent.
CREATE TABLE IF NOT EXISTS corp_kill_coverage (
    corporation_id BIGINT PRIMARY KEY,
    backfilled_at  TIMESTAMPTZ NOT NULL
);
