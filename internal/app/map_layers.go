package app

// ---------------------------------------------------------------------------
// The universe map's live layers (schema 073): what is happening in
// New Eden, drawn over the map. Five public datasets the intel pass
// already keeps warm in the global store --
//
//   kills      ship, pod and NPC kills per system, last hour
//   jumps      jumps into each system, last hour
//   sov        each system's holder: alliance, or NPC faction
//   fw         Faction Warfare occupancy and contest progress
//   incursions infested constellations, their systems and staging
//
// -- are folded by the worker into one compact payload after each
// intel pass, stored gzipped with its content hash, and served at a
// hashed URL. Pages never assemble it. A pass rebuilds only when a
// source moved: each snapshot's fetch time and the count of holding
// alliances with a resolved name make the fingerprint.
//
// Systems are keyed by ID, not by the universe payload's index, so
// the two payloads version independently; map.js joins them.
// ---------------------------------------------------------------------------

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// mapLayersPayload is the layers payload, as JSON. Mirrored as a
// JSDoc typedef in static/map.js; change both together.
type mapLayersPayload struct {
	Kills     mapLayerKills `json:"kills"`
	Jumps     mapLayerJumps `json:"jumps"`
	Sov       mapLayerSov   `json:"sov"`
	Alliances []mapNamed    `json:"alliances"`
	FW        mapLayerFW    `json:"fw"`
	Inc       mapLayerIncs  `json:"inc"`
	Factions  []mapNamed    `json:"factions"`
	// Threat is built from the killboard (map_threat.go), not from
	// a snapshot.
	Threat mapLayerThreat `json:"threat"`
}

// Every layer carries At: when its data was fetched (unix seconds,
// 0 when there is none yet).
type mapLayerKills struct {
	At   int64   `json:"at"`
	ID   []int64 `json:"id"`
	Ship []int64 `json:"ship"`
	Pod  []int64 `json:"pod"`
	NPC  []int64 `json:"npc"`
}

type mapLayerJumps struct {
	At int64   `json:"at"`
	ID []int64 `json:"id"`
	N  []int64 `json:"n"`
}

type mapLayerSov struct {
	At int64   `json:"at"`
	ID []int64 `json:"id"`
	A  []int   `json:"a"` // index into alliances, -1 for none
	F  []int64 `json:"f"` // NPC faction holder, 0 for none
}

type mapLayerFW struct {
	At    int64    `json:"at"`
	ID    []int64  `json:"id"`
	Occ   []int64  `json:"occ"`   // occupying faction
	Own   []int64  `json:"own"`   // owning faction
	State []string `json:"state"` // captured|contested|uncontested|vulnerable
	Pct   []int    `json:"pct"`   // contest progress, 0..100
}

type mapLayerIncs struct {
	At   int64          `json:"at"`
	List []mapIncursion `json:"list"`
}

type mapIncursion struct {
	Cons      int64   `json:"cons"`
	Staging   int64   `json:"staging"`
	Systems   []int64 `json:"sys"`
	State     string  `json:"state"`
	Influence float64 `json:"inf"`
	Boss      bool    `json:"boss"`
	Faction   int64   `json:"f"`
}

type mapNamed struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// mapLayerSources are the global snapshot kinds the layers read.
var mapLayerSources = []string{
	esi.GlobalSystemKills, esi.GlobalSystemJumps, esi.GlobalSovMap,
	esi.GlobalFWSystems, esi.GlobalIncursions, esi.GlobalFactions,
}

// refreshMapLayers rebuilds the stored layers payload when any of
// its sources moved since the last build (the worker calls it after
// each intel pass).
func (app *Application) refreshMapLayers(ctx context.Context) {
	snaps := map[string]db.GlobalSnapshot{}
	var fp []string
	for _, kind := range mapLayerSources {
		s, err := app.queries.GetGlobalSnapshot(ctx, kind)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				logging.Errorf("map: layers: read %s: %v", kind, err)
			}
			fp = append(fp, kind+"=")
			continue
		}
		snaps[kind] = s
		fp = append(fp, kind+"="+s.FetchedAt.UTC().Format(time.RFC3339))
	}
	now := time.Now().UTC()
	fp = append(fp, app.threatFingerprint(ctx, now))
	threat, _ := app.loadThreat(ctx, now)
	if len(snaps) == 0 && len(threat.ID) == 0 {
		return
	}
	payload, named := app.buildMapLayers(ctx, snaps)
	payload.Threat = threat
	fp = append(fp, fmt.Sprintf("names=%d", named))
	sources := strings.Join(fp, ";")
	if meta, err := app.queries.GetMapLayersMeta(ctx); err == nil && meta.Sources == sources {
		return
	}
	gz, etag, err := packMapJSON(payload)
	if err != nil {
		logging.Errorf("map: layers: %v", err)
		return
	}
	if err := app.queries.UpsertMapLayers(ctx, db.UpsertMapLayersParams{
		Sources: sources, Etag: etag, Payload: gz, BuiltAt: time.Now().UTC(),
	}); err != nil {
		logging.Errorf("map: layers: store: %v", err)
	}
}

// buildMapLayers folds the stored snapshots into the payload, and
// reports how many holding alliances it could name.
func (app *Application) buildMapLayers(ctx context.Context, snaps map[string]db.GlobalSnapshot) (mapLayersPayload, int) {
	p := mapLayersPayload{}
	decode := func(kind string, out any) int64 {
		s, ok := snaps[kind]
		if !ok || json.Unmarshal([]byte(s.Payload), out) != nil {
			return 0
		}
		return s.FetchedAt.Unix()
	}

	var kills esi.SystemKills
	if at := decode(esi.GlobalSystemKills, &kills); at > 0 {
		p.Kills.At = at
		sort.Slice(kills, func(i, j int) bool { return kills[i].SystemID < kills[j].SystemID })
		for _, k := range kills {
			if k.ShipKills+k.PodKills+k.NPCKills == 0 {
				continue
			}
			p.Kills.ID = append(p.Kills.ID, k.SystemID)
			p.Kills.Ship = append(p.Kills.Ship, k.ShipKills)
			p.Kills.Pod = append(p.Kills.Pod, k.PodKills)
			p.Kills.NPC = append(p.Kills.NPC, k.NPCKills)
		}
	}

	var jumps esi.SystemJumps
	if at := decode(esi.GlobalSystemJumps, &jumps); at > 0 {
		p.Jumps.At = at
		sort.Slice(jumps, func(i, j int) bool { return jumps[i].SystemID < jumps[j].SystemID })
		for _, j := range jumps {
			if j.ShipJumps == 0 {
				continue
			}
			p.Jumps.ID = append(p.Jumps.ID, j.SystemID)
			p.Jumps.N = append(p.Jumps.N, j.ShipJumps)
		}
	}

	named := 0
	var sov esi.SovMap
	if at := decode(esi.GlobalSovMap, &sov); at > 0 {
		p.Sov.At = at
		sort.Slice(sov, func(i, j int) bool { return sov[i].SystemID < sov[j].SystemID })
		allianceIdx := map[int64]int{}
		for _, s := range sov {
			if s.AllianceID == 0 && s.FactionID == 0 {
				continue
			}
			ai := -1
			if s.AllianceID > 0 {
				idx, ok := allianceIdx[s.AllianceID]
				if !ok {
					idx = len(p.Alliances)
					allianceIdx[s.AllianceID] = idx
					name, _ := app.resolvedAllianceName(ctx, s.AllianceID)
					if name != "" {
						named++
					}
					p.Alliances = append(p.Alliances, mapNamed{ID: s.AllianceID, Name: name})
				}
				ai = idx
			}
			p.Sov.ID = append(p.Sov.ID, s.SystemID)
			p.Sov.A = append(p.Sov.A, ai)
			p.Sov.F = append(p.Sov.F, s.FactionID)
		}
	}

	var fw esi.FWSystems
	if at := decode(esi.GlobalFWSystems, &fw); at > 0 {
		p.FW.At = at
		sort.Slice(fw, func(i, j int) bool { return fw[i].SolarSystemID < fw[j].SolarSystemID })
		for _, s := range fw {
			pct := 0
			if s.VictoryPointsThreshold > 0 {
				pct = int(min(100, s.VictoryPoints*100/s.VictoryPointsThreshold))
			}
			p.FW.ID = append(p.FW.ID, s.SolarSystemID)
			p.FW.Occ = append(p.FW.Occ, s.OccupierFactionID)
			p.FW.Own = append(p.FW.Own, s.OwnerFactionID)
			p.FW.State = append(p.FW.State, s.Contested)
			p.FW.Pct = append(p.FW.Pct, pct)
		}
	}

	var incs esi.Incursions
	if at := decode(esi.GlobalIncursions, &incs); at > 0 {
		p.Inc.At = at
		for _, inc := range incs {
			p.Inc.List = append(p.Inc.List, mapIncursion{
				Cons: inc.ConstellationID, Staging: inc.StagingSystemID, Systems: inc.InfestedSystems,
				State: inc.State, Influence: inc.Influence, Boss: inc.HasBoss, Faction: inc.FactionID,
			})
		}
	}

	var factions esi.Factions
	if decode(esi.GlobalFactions, &factions) > 0 {
		for _, f := range factions {
			p.Factions = append(p.Factions, mapNamed{ID: f.FactionID, Name: f.Name})
		}
	}
	return p, named
}

// handleMapLayersJSON serves the stored layers payload. The page's
// URL carries the etag it rendered with; map.js asks again with no
// version to pick up a newer build, which is cached briefly.
func (app *Application) handleMapLayersJSON(w http.ResponseWriter, r *http.Request) {
	row, err := app.queries.GetMapLayersPayload(r.Context())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "no map layers yet", http.StatusServiceUnavailable)
			return
		}
		logging.Errorf("map: read layers: %v", err)
		http.Error(w, "map layers unavailable", http.StatusInternalServerError)
		return
	}
	serveMapJSON(w, r, row.Etag, row.Payload, 60)
}
