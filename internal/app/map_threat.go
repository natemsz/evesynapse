package app

// ---------------------------------------------------------------------------
// The map's threat layer: how dangerous each system is right now,
// from the killboard's recent PvP kills, folded into the live layers
// payload (map_layers.go).
//
// Every PvP kill adds weight to its system that halves every
// threatHalfLife, so a system that was hot two hours ago has cooled on
// its own and one burning now reads hot. Every kill counts -- private
// ones too, since a score is a total and shows no kill. The map draws
// the score on fixed levels (quiet, low, medium, high, extreme), so a
// color means the same danger at any hour.
//
// A camp is a system where one group keeps killing passers-by:
// threatCampKills or more public kills in the last threatCampWindow,
// spread over at least threatCampSpan (one burst is a fight, not a
// camp), their victims from at least threatCampVictims corporations
// (one fleet dying together is a fight too), and the same alliance
// (else corporation, else pilot) on most of them. It is named from
// public kills only, so no private kill is ever revealed. Only ship
// kills are camp evidence: a structure or deployable dying (a tractor
// unit farmed at a gate) counts toward the system's heat, never
// toward a camp -- not its kill count, span, victim set or share.
//
// A camp also says where in the system it sits, when the kills'
// locations (zKill's nearest celestial for each victim) support
// it: one celestial holding most of the camp's located kills is
// named, kills split across celestials are reported as spread,
// and too little location evidence says nothing about place --
// a wrong gate name is worse than none. And it names its
// recurring crew: pilots on several of the camp's kills, whatever
// their tags say, which is what makes twenty kills a camp by
// known associates rather than twenty kills that happened in one
// system.
// ---------------------------------------------------------------------------

import (
	"context"
	"math"
	"sort"
	"time"

	"evesynapse/internal/logging"
)

const (
	threatHalfLife    = 45 * time.Minute
	threatWindow      = 3 * time.Hour // older kills add under 7% of a fresh one's weight
	threatCampWindow  = time.Hour
	threatCampKills   = 4
	threatCampSpan    = 10 * time.Minute
	threatCampVictims = 3
	// threatCampShare: the group on at least this share of the
	// window's public kills is the camp.
	threatCampShare = 0.6
	// threatCampCelestialMin: a camp's kills must carry a known
	// location at least this many times before the flag says
	// anything about place, either way.
	threatCampCelestialMin = 3
	// threatCampCelestialShare: one celestial holding at least
	// this share of the camp's located kills is where the camp
	// sits; under it, the kills are spread across the system.
	threatCampCelestialShare = 0.6
	// threatCampCrewMin: a pilot on at least this many of the
	// camp's kills is recurring crew.
	threatCampCrewMin = 2
	// threatCampCrewMax caps the crew a camp names.
	threatCampCrewMax = 5
)

// mapLayerThreat is the threat layer, systems keyed by ID.
type mapLayerThreat struct {
	At    int64     `json:"at"`
	ID    []int64   `json:"id"`
	Score []int     `json:"s"`    // decayed kill weight x100
	Hour  []int     `json:"k1"`   // kills in the last hour
	Last  []int64   `json:"last"` // the latest kill (unix)
	Camps []mapCamp `json:"camps"`
}

// mapCamp is one detected camp.
type mapCamp struct {
	System  int64  `json:"sys"`
	Kills   int    `json:"n"`
	Kind    string `json:"kind"` // "alliance" | "corporation" | "character"
	GroupID int64  `json:"gid"`
	Group   string `json:"group"`
	Since   int64  `json:"since"` // the window's first kill (unix)
	// CelestialID is the celestial most of the camp's located
	// kills point at, when one dominates; Celestial is its name,
	// resolved by the caller (dressCamps). Spread is set when
	// the located kills split across celestials instead.
	CelestialID int64    `json:"-"`
	Celestial   string   `json:"cel,omitempty"`
	Spread      bool     `json:"spread,omitempty"`
	CrewIDs     []int64  `json:"-"`
	Crew        []string `json:"crew,omitempty"`
}

type threatKill struct {
	ID         int64
	System     int64
	Time       time.Time
	Public     bool
	VictimCorp int64
	// Location: the celestial the victim died at (kills.location_id,
	// zKill's nearest celestial), 0 when no source supplied one.
	// Kills only ESI ever carried have none: the killmail's victim
	// position is not stored, so those kills never vote on where
	// a camp sits.
	Location int64
	// Structure: the victim was a structure or deployable, by SDE
	// category. Such kills are heat, not camp evidence.
	Structure bool
}

type threatAttacker struct {
	Kill                      int64
	Character, Corp, Alliance int64
}

// buildThreat scores every system with a recent PvP kill and finds
// the camps. name resolves a camp group's name (kind, id).
func buildThreat(now time.Time, kills []threatKill, attackers []threatAttacker, name func(kind string, id int64) string) mapLayerThreat {
	t := mapLayerThreat{At: now.Unix()}
	type acc struct {
		score float64
		hour  int
		last  time.Time
	}
	by := map[int64]*acc{}
	recent := map[int64][]threatKill{} // public kills in the camp window, per system
	for _, k := range kills {
		age := now.Sub(k.Time)
		if age < 0 {
			age = 0
		}
		if age > threatWindow {
			continue
		}
		a := by[k.System]
		if a == nil {
			a = &acc{}
			by[k.System] = a
		}
		a.score += math.Pow(0.5, float64(age)/float64(threatHalfLife))
		if age <= time.Hour {
			a.hour++
		}
		if k.Time.After(a.last) {
			a.last = k.Time
		}
		if k.Public && !k.Structure && age <= threatCampWindow {
			recent[k.System] = append(recent[k.System], k)
		}
	}
	ids := make([]int64, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		a := by[id]
		t.ID = append(t.ID, id)
		t.Score = append(t.Score, int(math.Round(a.score*100)))
		t.Hour = append(t.Hour, a.hour)
		t.Last = append(t.Last, a.last.Unix())
	}

	// Camps: per system, which group sits on most of its recent
	// public kills.
	byKill := map[int64][]threatAttacker{}
	for _, a := range attackers {
		byKill[a.Kill] = append(byKill[a.Kill], a)
	}
	systems := make([]int64, 0, len(recent))
	for s, ks := range recent {
		if len(ks) >= threatCampKills {
			systems = append(systems, s)
		}
	}
	sort.Slice(systems, func(i, j int) bool { return systems[i] < systems[j] })
	for _, s := range systems {
		ks := recent[s]
		locOf := map[int64]int64{}
		for _, k := range ks {
			locOf[k.ID] = k.Location
		}
		first, last := ks[0].Time, ks[0].Time
		victims := map[int64]bool{}
		for _, k := range ks {
			if k.Time.Before(first) {
				first = k.Time
			}
			if k.Time.After(last) {
				last = k.Time
			}
			victims[k.VictimCorp] = true
		}
		if last.Sub(first) < threatCampSpan || len(victims) < threatCampVictims {
			continue // one burst, or one fleet: a fight
		}
		type key struct {
			kind string
			id   int64
		}
		count := map[key]int{}
		groupKills := map[key][]int64{}
		for _, k := range ks {
			seen := map[key]bool{}
			for _, a := range byKill[k.ID] {
				g := key{"character", a.Character}
				switch {
				case a.Alliance > 0:
					g = key{"alliance", a.Alliance}
				case a.Corp >= 98000000: // a player corporation, not an NPC one
					g = key{"corporation", a.Corp}
				}
				if !seen[g] {
					seen[g] = true
					count[g]++
					groupKills[g] = append(groupKills[g], k.ID)
				}
			}
		}
		var best key
		bestN := 0
		for g, n := range count {
			if n > bestN || (n == bestN && g.id < best.id) {
				best, bestN = g, n
			}
		}
		if bestN < threatCampKills || float64(bestN) < threatCampShare*float64(len(ks)) {
			continue
		}
		camp := mapCamp{
			System: s, Kills: bestN, Kind: best.kind, GroupID: best.id,
			Group: name(best.kind, best.id), Since: first.Unix(),
		}
		// Where the camp sits: the group's kills by location.
		// Kills without a location do not vote, either way.
		byLoc := map[int64]int{}
		located := 0
		for _, id := range groupKills[best] {
			if loc := locOf[id]; loc > 0 {
				byLoc[loc]++
				located++
			}
		}
		if located >= threatCampCelestialMin {
			top, topN := int64(0), 0
			for loc, n := range byLoc {
				if n > topN || (n == topN && loc < top) {
					top, topN = loc, n
				}
			}
			if float64(topN) >= threatCampCelestialShare*float64(located) {
				camp.CelestialID = top
			} else {
				camp.Spread = true
			}
		}
		// The recurring crew: pilots on several of the camp's
		// kills, whatever their tags say.
		appear := map[int64]int{}
		for _, id := range groupKills[best] {
			seenPilot := map[int64]bool{}
			for _, a := range byKill[id] {
				if a.Character > 0 && !seenPilot[a.Character] {
					seenPilot[a.Character] = true
					appear[a.Character]++
				}
			}
		}
		for pid, n := range appear {
			if n >= threatCampCrewMin {
				camp.CrewIDs = append(camp.CrewIDs, pid)
			}
		}
		sort.Slice(camp.CrewIDs, func(i, j int) bool {
			if appear[camp.CrewIDs[i]] != appear[camp.CrewIDs[j]] {
				return appear[camp.CrewIDs[i]] > appear[camp.CrewIDs[j]]
			}
			return camp.CrewIDs[i] < camp.CrewIDs[j]
		})
		if len(camp.CrewIDs) > threatCampCrewMax {
			camp.CrewIDs = camp.CrewIDs[:threatCampCrewMax]
		}
		t.Camps = append(t.Camps, camp)
	}
	return t
}

// structureCategory reports whether an SDE category is a structure
// or a deployable: what a gate's campers can farm without ever
// killing a ship (tractor units, depots, siphons, inhibitors,
// Upwell structures, starbase pieces).
func structureCategory(categoryID int64) bool {
	switch categoryID {
	case catDeployable, catStarbase, catStructure:
		return true
	}
	return false
}

// structureTypes resolves which of the given victim ship types are
// structures or deployables, in one batched SDE category read. A
// type the SDE does not classify counts as a ship.
func (app *Application) structureTypes(ctx context.Context, typeIDs []int64) map[int64]bool {
	out := map[int64]bool{}
	seen := map[int64]bool{}
	ids := make([]int64, 0, len(typeIDs))
	for _, id := range typeIDs {
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out
	}
	rows, err := app.queries.ListSDETypeCategories(ctx, ids)
	if err != nil {
		logging.Errorf("map: threat: type categories: %v", err)
		return out
	}
	for _, r := range rows {
		if structureCategory(r.CategoryID) {
			out[r.TypeID] = true
		}
	}
	return out
}

// loadThreat reads the recent kills and builds the threat layer.
func (app *Application) loadThreat(ctx context.Context, now time.Time) (mapLayerThreat, bool) {
	since := now.Add(-threatWindow)
	rows, err := app.queries.ListThreatKills(ctx, since)
	if err != nil {
		logging.Errorf("map: threat: kills: %v", err)
		return mapLayerThreat{}, false
	}
	types := make([]int64, len(rows))
	for i, r := range rows {
		types[i] = r.ShipTypeID
	}
	structs := app.structureTypes(ctx, types)
	kills := make([]threatKill, len(rows))
	for i, r := range rows {
		kills[i] = threatKill{ID: r.KillmailID, System: r.SystemID, Time: r.KillmailTime, Public: r.Public, VictimCorp: r.VictimCorporationID, Location: r.LocationID, Structure: structs[r.ShipTypeID]}
	}
	arows, err := app.queries.ListThreatAttackers(ctx, now.Add(-threatCampWindow))
	if err != nil {
		logging.Errorf("map: threat: attackers: %v", err)
	}
	atk := make([]threatAttacker, len(arows))
	for i, a := range arows {
		atk[i] = threatAttacker{Kill: a.KillmailID, Character: a.CharacterID, Corp: a.CorporationID, Alliance: a.AllianceID}
	}
	names := app.newKillNames(ctx)
	th := buildThreat(now, kills, atk, func(kind string, id int64) string {
		switch kind {
		case "alliance":
			return names.alliance(id)
		case "corporation":
			return names.corp(id)
		}
		return names.character(id)
	})
	app.dressCamps(ctx, names, th.Camps)
	return th, true
}

// threatFingerprint moves when a kill lands, and every five minutes
// regardless, so decay redraws a cooling system.
func (app *Application) threatFingerprint(ctx context.Context, now time.Time) string {
	latest := time.Time{}
	if r, err := app.queries.GetLatestKillIngest(ctx); err == nil {
		latest = r
	}
	return "threat=" + latest.UTC().Format(time.RFC3339) + "/" + now.UTC().Truncate(5*time.Minute).Format(time.RFC3339)
}
