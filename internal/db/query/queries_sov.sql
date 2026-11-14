-- The sovereignty tracker (intel_sov_page.go). Every read is
-- bounded: one summary row, and lists capped by max_rows. A region
-- or alliance of 0 means all.

-- name: SovSummary :one
SELECT
    count(*) FILTER (WHERE s.alliance_id > 0)::bigint AS held,
    count(DISTINCT s.alliance_id) FILTER (WHERE s.alliance_id > 0)::bigint AS alliances,
    count(*) FILTER (WHERE s.alliance_id > 0 AND s.claimed_since >= sqlc.arg(since_day)::timestamptz)::bigint AS claimed_day,
    count(*) FILTER (WHERE s.alliance_id > 0 AND s.claimed_since >= sqlc.arg(since_week)::timestamptz)::bigint AS claimed_week,
    count(*) FILTER (WHERE s.alliance_id > 0 AND s.vuln_start <= sqlc.arg(now)::timestamptz AND s.vuln_end > sqlc.arg(now)::timestamptz)::bigint AS vulnerable_now,
    count(*) FILTER (WHERE s.alliance_id > 0 AND s.vuln_start > sqlc.arg(now)::timestamptz AND s.vuln_start < sqlc.arg(until)::timestamptz)::bigint AS vulnerable_next,
    (SELECT count(*) FROM sov_campaigns c
       JOIN sde_systems cs ON cs.system_id = c.system_id
     WHERE (sqlc.arg(region_id)::bigint = 0 OR cs.region_id = sqlc.arg(region_id)::bigint)
       AND (sqlc.arg(alliance_id)::bigint = 0 OR c.defender_id = sqlc.arg(alliance_id)::bigint))::bigint AS contests
FROM system_sovereignty s
JOIN sde_systems ss ON ss.system_id = s.system_id
WHERE (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(alliance_id)::bigint = 0 OR s.alliance_id = sqlc.arg(alliance_id)::bigint);

-- name: ListSovContests :many
SELECT c.campaign_id, c.system_id, ss.name AS system_name, ss.security, ss.region_id,
       COALESCE(r.name, '')::text AS region_name, c.event_type, c.defender_id,
       c.defender_score, c.attackers_score, c.start_time,
       COALESCE(s.adm, 0)::double precision AS adm
FROM sov_campaigns c
JOIN sde_systems ss ON ss.system_id = c.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
LEFT JOIN system_sovereignty s ON s.system_id = c.system_id
WHERE (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(alliance_id)::bigint = 0 OR c.defender_id = sqlc.arg(alliance_id)::bigint)
ORDER BY c.start_time, c.campaign_id
LIMIT sqlc.arg(max_rows)::bigint;

-- Claims taken since a time, newest first, each with the holder the
-- change log last saw before it (0 when the log predates the claim).
-- The previous holder is the claim row's own old holder; when that
-- row records none (the system sat unclaimed first), it is the old
-- holder of the most recent logged change for the system that names
-- one, which is the drop that left it unclaimed. A drop by the
-- alliance that holds it now is a reclaim: no previous holder.
-- Rows are not matched by time: a claim row is dated by its
-- claimed_since, which can precede the moment the drop before it
-- was seen.
-- name: ListSovRecentClaims :many
SELECT s.system_id, ss.name AS system_name, ss.security, ss.region_id,
       COALESCE(r.name, '')::text AS region_name, s.alliance_id, s.claimed_since,
       s.adm, s.is_capital,
       COALESCE(prev.old_alliance_id, 0)::bigint AS prev_alliance_id,
       COALESCE(prev.old_faction_id, 0)::bigint AS prev_faction_id,
       (prev.id IS NOT NULL)::boolean AS logged
FROM system_sovereignty s
JOIN sde_systems ss ON ss.system_id = s.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
LEFT JOIN LATERAL (
    SELECT cl.id,
           COALESCE(NULLIF(cl.old_alliance_id, 0), NULLIF(dr.old_alliance_id, s.alliance_id), 0) AS old_alliance_id,
           COALESCE(NULLIF(cl.old_faction_id, 0), CASE WHEN dr.old_alliance_id = s.alliance_id THEN 0 ELSE dr.old_faction_id END, 0) AS old_faction_id
    FROM (
        SELECT c.id, c.changed_at, c.old_alliance_id, c.old_faction_id FROM sov_changes c
        WHERE c.system_id = s.system_id AND c.new_alliance_id = s.alliance_id
        ORDER BY c.changed_at DESC, c.id DESC LIMIT 1
    ) cl
    LEFT JOIN LATERAL (
        SELECT d.old_alliance_id, d.old_faction_id FROM sov_changes d
        WHERE d.system_id = s.system_id AND d.id <> cl.id
          AND (d.old_alliance_id > 0 OR d.old_faction_id > 0)
        ORDER BY d.changed_at DESC, d.id DESC LIMIT 1
    ) dr ON cl.old_alliance_id = 0 AND cl.old_faction_id = 0
) prev ON true
WHERE s.alliance_id > 0 AND s.claimed_since >= sqlc.arg(since)::timestamptz
  AND (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(alliance_id)::bigint = 0 OR s.alliance_id = sqlc.arg(alliance_id)::bigint)
ORDER BY s.claimed_since DESC, s.system_id
LIMIT sqlc.arg(max_rows)::bigint;

-- Claims lost since a time (from the change log), newest first.
-- name: ListSovLosses :many
SELECT c.system_id, ss.name AS system_name, ss.security, ss.region_id,
       COALESCE(r.name, '')::text AS region_name, c.changed_at,
       c.old_alliance_id, c.new_alliance_id, c.new_faction_id
FROM sov_changes c
JOIN sde_systems ss ON ss.system_id = c.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
WHERE c.changed_at >= sqlc.arg(since)::timestamptz AND c.old_alliance_id > 0
  AND c.old_alliance_id <> c.new_alliance_id
  AND (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(alliance_id)::bigint = 0 OR c.old_alliance_id = sqlc.arg(alliance_id)::bigint)
ORDER BY c.changed_at DESC, c.id DESC
LIMIT sqlc.arg(max_rows)::bigint;

-- Hubs whose vulnerability window is open now or opens before
-- until, the open ones first, then by opening time.
-- name: ListSovVulnerable :many
SELECT s.system_id, ss.name AS system_name, ss.security, ss.region_id,
       COALESCE(r.name, '')::text AS region_name, s.alliance_id,
       s.vuln_start, s.vuln_end, s.adm, s.is_capital,
       s.military_level, s.industrial_level, s.strategic_level
FROM system_sovereignty s
JOIN sde_systems ss ON ss.system_id = s.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
WHERE s.alliance_id > 0 AND s.vuln_end > sqlc.arg(now)::timestamptz
  AND s.vuln_start < sqlc.arg(until)::timestamptz
  -- mode 1: open now only; mode 2: opening later only; 0: both.
  AND (sqlc.arg(mode)::int = 0
       OR (sqlc.arg(mode)::int = 1 AND s.vuln_start <= sqlc.arg(now)::timestamptz)
       OR (sqlc.arg(mode)::int = 2 AND s.vuln_start > sqlc.arg(now)::timestamptz))
  AND (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
  AND (sqlc.arg(alliance_id)::bigint = 0 OR s.alliance_id = sqlc.arg(alliance_id)::bigint)
ORDER BY (s.vuln_start <= sqlc.arg(now)::timestamptz) DESC, s.vuln_start, s.adm, s.system_id
LIMIT sqlc.arg(max_rows)::bigint;

-- The alliances holding sovereignty, most systems first: systems
-- held, claims taken and lost since a time, average ADM and their
-- capital count.
-- name: ListSovAlliances :many
WITH held AS (
    SELECT s.alliance_id, count(*)::bigint AS held,
           count(*) FILTER (WHERE s.claimed_since >= sqlc.arg(since)::timestamptz)::bigint AS gained,
           avg(s.adm)::double precision AS avg_adm,
           count(*) FILTER (WHERE s.is_capital)::bigint AS capitals
    FROM system_sovereignty s
    JOIN sde_systems ss ON ss.system_id = s.system_id
    WHERE s.alliance_id > 0
      AND (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
    GROUP BY s.alliance_id
), lost AS (
    SELECT c.old_alliance_id AS alliance_id, count(*)::bigint AS lost
    FROM sov_changes c
    JOIN sde_systems ss ON ss.system_id = c.system_id
    WHERE c.changed_at >= sqlc.arg(since)::timestamptz AND c.old_alliance_id > 0
      AND c.old_alliance_id <> c.new_alliance_id
      AND (sqlc.arg(region_id)::bigint = 0 OR ss.region_id = sqlc.arg(region_id)::bigint)
    GROUP BY c.old_alliance_id
)
SELECT h.alliance_id, h.held, h.gained, COALESCE(l.lost, 0)::bigint AS lost, h.avg_adm, h.capitals
FROM held h LEFT JOIN lost l ON l.alliance_id = h.alliance_id
ORDER BY h.held DESC, h.alliance_id
LIMIT sqlc.arg(max_rows)::bigint;

-- The regions where any alliance holds sovereignty, for the filter.
-- name: ListSovRegions :many
SELECT r.region_id, r.name FROM sde_regions r
WHERE EXISTS (
    SELECT 1 FROM system_sovereignty s JOIN sde_systems ss ON ss.system_id = s.system_id
    WHERE ss.region_id = r.region_id AND s.alliance_id > 0
)
ORDER BY r.name;

-- One alliance's systems, its capital first, then by ADM.
-- name: ListAllianceSovSystems :many
SELECT s.system_id, ss.name AS system_name, ss.security, ss.region_id,
       COALESCE(r.name, '')::text AS region_name, s.claimed_since,
       s.vuln_start, s.vuln_end, s.adm, s.is_capital
FROM system_sovereignty s
JOIN sde_systems ss ON ss.system_id = s.system_id
LEFT JOIN sde_regions r ON r.region_id = ss.region_id
WHERE s.alliance_id = sqlc.arg(alliance_id)::bigint
ORDER BY s.is_capital DESC, s.adm DESC, ss.name
LIMIT sqlc.arg(max_rows)::bigint;

-- When the change log began: losses are known from then on.
-- name: SovChangeLogStart :one
SELECT min(changed_at)::timestamptz AS first FROM sov_changes;

-- One alliance's sovereignty at a glance: systems held, claims
-- taken and lost since a time, and its capital system.
-- name: AllianceSovSummary :one
SELECT
    (SELECT count(*) FROM system_sovereignty s WHERE s.alliance_id = sqlc.arg(alliance_id)::bigint)::bigint AS held,
    (SELECT count(*) FROM system_sovereignty s WHERE s.alliance_id = sqlc.arg(alliance_id)::bigint
        AND s.claimed_since >= sqlc.arg(since)::timestamptz)::bigint AS gained,
    (SELECT count(*) FROM sov_changes c WHERE c.old_alliance_id = sqlc.arg(alliance_id)::bigint
        AND c.new_alliance_id <> c.old_alliance_id AND c.changed_at >= sqlc.arg(since)::timestamptz)::bigint AS lost,
    COALESCE((SELECT avg(s.adm) FROM system_sovereignty s WHERE s.alliance_id = sqlc.arg(alliance_id)::bigint), 0)::double precision AS avg_adm;
