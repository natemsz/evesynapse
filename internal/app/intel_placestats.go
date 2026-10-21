package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lib/pq"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Place activity collection (schema 075). ESI publishes the whole
// universe's kills and jumps as two public aggregates, refreshed
// hourly, and the sovereignty map on the same kind of window. This
// pass mirrors all three into stored tables on ESI's own cache
// window -- the guide-prices pattern, gated by place_stats_meta --
// so the system and region pages render from stored rows only.
//
// The kills/jumps payloads land as one history row per system per
// collected hour (30-day retention, pruned here), and each pass
// re-distills the page-ready figures (last hour, trailing 24h,
// trailing-7d daily averages over the hours of collection) into
// system_activity_stats and region_activity_stats. Sovereignty is
// a current-state mirror, replaced wholesale. A failed pass stores
// nothing: the pages keep the last good figures, never blanks.
// ---------------------------------------------------------------------------

// place_stats_meta kinds, one gate per dataset.
const (
	placeStatsKindActivity    = "activity"
	placeStatsKindSovereignty = "sovereignty"
)

// placeActivityRetention bounds the hourly history the averages
// are distilled from; the distillation window itself is 7 days.
const placeActivityRetention = 30 * 24 * time.Hour

// placeStatsDue reports whether a dataset's cache window has
// closed. No gate row yet means due.
func (app *Application) placeStatsDue(ctx context.Context, kind string, now time.Time) bool {
	meta, err := app.queries.GetPlaceStatsMeta(ctx, kind)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("worker: place stats gate %s: %v", kind, err)
		}
		return true
	}
	return !now.Before(meta.CachedUntil)
}

// placeCacheUntil reads ESI's Expires header, with an hour's
// fallback when it is missing or unparseable.
func placeCacheUntil(header http.Header, now time.Time) time.Time {
	until := now.Add(time.Hour)
	if exp := header.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			until = t
		}
	}
	return until
}

// esiSystemKills is one row of GET /universe/system_kills/: the
// system's kills over the hour the response covers.
type esiSystemKills struct {
	SystemID  int64 `json:"system_id"`
	ShipKills int64 `json:"ship_kills"`
	NPCKills  int64 `json:"npc_kills"`
	PodKills  int64 `json:"pod_kills"`
}

// esiSystemJumps is one row of GET /universe/system_jumps/.
type esiSystemJumps struct {
	SystemID  int64 `json:"system_id"`
	ShipJumps int64 `json:"ship_jumps"`
}

// refreshPlaceActivity mirrors the kills and jumps aggregates
// into the hourly history and re-distills the page figures.
// Reports whether it stored, and whether ESI's error limit
// stopped it.
func (app *Application) refreshPlaceActivity(ctx context.Context) (stored bool, limited bool) {
	now := time.Now()
	if !app.placeStatsDue(ctx, placeStatsKindActivity, now) {
		return false, false // still inside ESI's cache window
	}

	killsBody, killsHeader, err := app.esi.FetchRaw(ctx, "", "/universe/system_kills/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return false, true
		}
		if ctx.Err() == nil {
			logging.Errorf("worker: place activity kills: %v", err)
		}
		return false, false
	}
	jumpsBody, jumpsHeader, err := app.esi.FetchRaw(ctx, "", "/universe/system_jumps/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return false, true
		}
		if ctx.Err() == nil {
			logging.Errorf("worker: place activity jumps: %v", err)
		}
		return false, false
	}
	var kills []esiSystemKills
	if err := json.Unmarshal(killsBody, &kills); err != nil {
		logging.Errorf("worker: place activity kills: decode: %v", err)
		return false, false
	}
	var jumps []esiSystemJumps
	if err := json.Unmarshal(jumpsBody, &jumps); err != nil {
		logging.Errorf("worker: place activity jumps: decode: %v", err)
		return false, false
	}

	// One history row per system named by either payload; a system
	// absent from one had none of that kind this hour.
	type activityRow struct{ ship, npc, pod, jumps int64 }
	merged := make(map[int64]*activityRow, len(kills)+len(jumps))
	for _, k := range kills {
		if k.SystemID <= 0 {
			continue
		}
		row := merged[k.SystemID]
		if row == nil {
			row = &activityRow{}
			merged[k.SystemID] = row
		}
		row.ship, row.npc, row.pod = k.ShipKills, k.NPCKills, k.PodKills
	}
	for _, j := range jumps {
		if j.SystemID <= 0 {
			continue
		}
		row := merged[j.SystemID]
		if row == nil {
			row = &activityRow{}
			merged[j.SystemID] = row
		}
		row.jumps = j.ShipJumps
	}

	cachedUntil := placeCacheUntil(killsHeader, now)
	if u := placeCacheUntil(jumpsHeader, now); u.Before(cachedUntil) {
		cachedUntil = u // the earlier window governs the next pass
	}
	hour := now.UTC().Truncate(time.Hour)

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: place activity: begin tx: %v", err)
		return false, false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)

	if len(merged) > 0 {
		n := len(merged)
		systemIDs := make([]int64, 0, n)
		shipKills := make([]int64, 0, n)
		npcKills := make([]int64, 0, n)
		podKills := make([]int64, 0, n)
		shipJumps := make([]int64, 0, n)
		for id, row := range merged {
			systemIDs = append(systemIDs, id)
			shipKills = append(shipKills, row.ship)
			npcKills = append(npcKills, row.npc)
			podKills = append(podKills, row.pod)
			shipJumps = append(shipJumps, row.jumps)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_activity_hours (system_id, hour, ship_kills, npc_kills, pod_kills, ship_jumps)
			SELECT unnest($1::bigint[]), $2::timestamptz, unnest($3::bigint[]),
			       unnest($4::bigint[]), unnest($5::bigint[]), unnest($6::bigint[])
			ON CONFLICT (system_id, hour) DO UPDATE SET
				ship_kills = EXCLUDED.ship_kills, npc_kills = EXCLUDED.npc_kills,
				pod_kills = EXCLUDED.pod_kills, ship_jumps = EXCLUDED.ship_jumps
		`, pq.Array(systemIDs), hour, pq.Array(shipKills), pq.Array(npcKills), pq.Array(podKills), pq.Array(shipJumps)); err != nil {
			logging.Errorf("worker: place activity: store history: %v", err)
			return false, false
		}
	}
	if _, err := qtx.PruneSystemActivityHours(ctx, now.UTC().Add(-placeActivityRetention)); err != nil {
		logging.Errorf("worker: place activity: prune history: %v", err)
		return false, false
	}
	if err := distillPlaceActivity(ctx, qtx, now.UTC()); err != nil {
		logging.Errorf("worker: place activity: distill: %v", err)
		return false, false
	}
	if err := qtx.UpsertPlaceStatsMeta(ctx, db.UpsertPlaceStatsMetaParams{
		Kind: placeStatsKindActivity, FetchedAt: now.UTC(), CachedUntil: cachedUntil.UTC(),
	}); err != nil {
		logging.Errorf("worker: place activity: store meta: %v", err)
		return false, false
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: place activity: commit: %v", err)
		return false, false
	}
	logging.Infof("worker: place activity stored (%d systems, fresh until %s)",
		len(merged), cachedUntil.UTC().Format(time.RFC3339))
	return true, false
}

// distillPlaceActivity rebuilds both stats tables from the hourly
// history as of now. Shared by the collector pass and the tests.
func distillPlaceActivity(ctx context.Context, q *db.Queries, now time.Time) error {
	if err := q.ClearSystemActivityStats(ctx); err != nil {
		return err
	}
	if err := q.DistillSystemActivityStats(ctx, now); err != nil {
		return err
	}
	if err := q.ClearRegionActivityStats(ctx); err != nil {
		return err
	}
	return q.DistillRegionActivityStats(ctx, now)
}

// esiSovereigntySystem is one row of GET /sovereignty/map/: the
// system's current holder. Fields beyond system_id are absent
// when they do not apply (an unallied corporation has no
// alliance_id; an NPC-held system carries faction_id instead).
type esiSovereigntySystem struct {
	SystemID      int64 `json:"system_id"`
	AllianceID    int64 `json:"alliance_id"`
	CorporationID int64 `json:"corporation_id"`
	FactionID     int64 `json:"faction_id"`
}

// refreshPlaceSovereignty mirrors the sovereignty map into
// system_sovereignty, replacing the previous set wholesale: a
// system the payload no longer names has lost its holder.
// Reports whether it stored, and whether ESI's error limit
// stopped it.
func (app *Application) refreshPlaceSovereignty(ctx context.Context) (stored bool, limited bool) {
	now := time.Now()
	if !app.placeStatsDue(ctx, placeStatsKindSovereignty, now) {
		return false, false // still inside ESI's cache window
	}

	body, header, err := app.esi.FetchRaw(ctx, "", "/sovereignty/map/")
	if err != nil {
		if errors.Is(err, esi.ErrErrorLimit) {
			return false, true
		}
		if ctx.Err() == nil {
			logging.Errorf("worker: place sovereignty: %v", err)
		}
		return false, false
	}
	var systems []esiSovereigntySystem
	if err := json.Unmarshal(body, &systems); err != nil {
		logging.Errorf("worker: place sovereignty: decode: %v", err)
		return false, false
	}

	// Faction holders need names, and there is no stored
	// faction-name tier: resolve them in this pass from the one
	// public factions call. If that call fails, the previous
	// pass's stored names carry over by system.
	factionNames := make(map[int64]string)
	needsFactions := false
	for _, s := range systems {
		if s.FactionID > 0 {
			needsFactions = true
			break
		}
	}
	if needsFactions {
		var factions []esi.Faction
		if err := app.esi.Get(ctx, "", "/universe/factions/", &factions); err != nil {
			if ctx.Err() == nil {
				logging.Errorf("worker: place sovereignty factions: %v (keeping stored names)", err)
			}
			if prev, perr := app.queries.ListSystemSovereigntyFactionNames(ctx); perr == nil {
				for _, row := range prev {
					factionNames[row.SystemID] = row.FactionName
				}
			}
		} else {
			byID := make(map[int64]string, len(factions))
			for _, f := range factions {
				byID[f.FactionID] = f.Name
			}
			for _, s := range systems {
				if s.FactionID > 0 {
					factionNames[s.SystemID] = byID[s.FactionID]
				}
			}
		}
	}

	cachedUntil := placeCacheUntil(header, now)

	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		logging.Errorf("worker: place sovereignty: begin tx: %v", err)
		return false, false
	}
	defer tx.Rollback()
	qtx := app.queries.WithTx(tx)
	if err := qtx.ClearSystemSovereignty(ctx); err != nil {
		logging.Errorf("worker: place sovereignty: clear: %v", err)
		return false, false
	}
	if len(systems) > 0 {
		systemIDs := make([]int64, 0, len(systems))
		allianceIDs := make([]int64, 0, len(systems))
		corpIDs := make([]int64, 0, len(systems))
		factionIDs := make([]int64, 0, len(systems))
		factionNameList := make([]string, 0, len(systems))
		seen := make(map[int64]bool, len(systems))
		for _, s := range systems {
			if s.SystemID <= 0 || seen[s.SystemID] {
				continue
			}
			seen[s.SystemID] = true
			systemIDs = append(systemIDs, s.SystemID)
			allianceIDs = append(allianceIDs, s.AllianceID)
			corpIDs = append(corpIDs, s.CorporationID)
			factionIDs = append(factionIDs, s.FactionID)
			factionNameList = append(factionNameList, factionNames[s.SystemID])
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_sovereignty (system_id, alliance_id, corporation_id, faction_id, faction_name, updated_at)
			SELECT unnest($1::bigint[]), unnest($2::bigint[]), unnest($3::bigint[]),
			       unnest($4::bigint[]), unnest($5::text[]), $6::timestamptz
		`, pq.Array(systemIDs), pq.Array(allianceIDs), pq.Array(corpIDs),
			pq.Array(factionIDs), pq.Array(factionNameList), now.UTC()); err != nil {
			logging.Errorf("worker: place sovereignty: store: %v", err)
			return false, false
		}
	}
	if err := qtx.UpsertPlaceStatsMeta(ctx, db.UpsertPlaceStatsMetaParams{
		Kind: placeStatsKindSovereignty, FetchedAt: now.UTC(), CachedUntil: cachedUntil.UTC(),
	}); err != nil {
		logging.Errorf("worker: place sovereignty: store meta: %v", err)
		return false, false
	}
	if err := tx.Commit(); err != nil {
		logging.Errorf("worker: place sovereignty: commit: %v", err)
		return false, false
	}
	logging.Infof("worker: place sovereignty stored (%d systems, fresh until %s)",
		len(systems), cachedUntil.UTC().Format(time.RFC3339))
	return true, false
}
