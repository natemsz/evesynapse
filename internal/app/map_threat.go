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
// public kills only, so no private kill is ever revealed.
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
}

type threatKill struct {
	ID         int64
	System     int64
	Time       time.Time
	Public     bool
	VictimCorp int64
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
		if k.Public && age <= threatCampWindow {
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
		t.Camps = append(t.Camps, mapCamp{
			System: s, Kills: bestN, Kind: best.kind, GroupID: best.id,
			Group: name(best.kind, best.id), Since: first.Unix(),
		})
	}
	return t
}

// loadThreat reads the recent kills and builds the threat layer.
func (app *Application) loadThreat(ctx context.Context, now time.Time) (mapLayerThreat, bool) {
	since := now.Add(-threatWindow)
	rows, err := app.queries.ListThreatKills(ctx, since)
	if err != nil {
		logging.Errorf("map: threat: kills: %v", err)
		return mapLayerThreat{}, false
	}
	kills := make([]threatKill, len(rows))
	for i, r := range rows {
		kills[i] = threatKill{ID: r.KillmailID, System: r.SystemID, Time: r.KillmailTime, Public: r.Public, VictimCorp: r.VictimCorporationID}
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
	return buildThreat(now, kills, atk, func(kind string, id int64) string {
		switch kind {
		case "alliance":
			return names.alliance(id)
		case "corporation":
			return names.corp(id)
		}
		return names.character(id)
	}), true
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
