package app

// ---------------------------------------------------------------------------
// The map's system card, kills and camp (GET /map/system.json?system=):
// the data behind the kills and threat layers, for one system, so the
// player can see what made a system hot without leaving the map.
//
// kills is the system's latest public kills, newest first, with a
// link to the killboard for the rest. camp, when the system has one
// right now, is the camp the threat layer marks (map_threat.go), found
// the same way from this system's kills alone: who is camping, the
// ships they fly, how many pilots, and their victims. Public kills
// only, like the killboard; answered from a short cache per system.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	mapSystemKills = 6
	mapSystemTTL   = 30 * time.Second
)

type mapSystemKill struct {
	ID     int64  `json:"id"`
	Ship   string `json:"ship"`
	ShipID int64  `json:"shipID"`
	Victim string `json:"victim"`
	Group  string `json:"group,omitempty"` // the victim's alliance, else corporation
	Value  string `json:"value,omitempty"`
	At     int64  `json:"at"`
	Solo   bool   `json:"solo,omitempty"`
	NPC    bool   `json:"npc,omitempty"`
}

type mapSystemShip struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	N    int    `json:"n"`
}

type mapSystemCamp struct {
	Kind    string          `json:"kind"`
	GroupID int64           `json:"gid"`
	Group   string          `json:"group"`
	Href    string          `json:"href"` // the group on the killboard
	Kills   int             `json:"n"`
	Since   int64           `json:"since"`
	Pilots  int             `json:"pilots"`
	Ships   []mapSystemShip `json:"ships"`
	Victims []mapSystemKill `json:"victims"`
}

type mapSystemData struct {
	Kills []mapSystemKill `json:"kills"`
	More  string          `json:"more"`
	Camp  *mapSystemCamp  `json:"camp,omitempty"`
}

type mapSystemKey struct {
	app *Application
	sys int64
}

type mapSystemEntry struct {
	at   time.Time
	body []byte
}

var mapSystemCache sync.Map // mapSystemKey -> mapSystemEntry

// handleMapSystem is GET /map/system.json?system=.
func (app *Application) handleMapSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sys, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64)
	if err != nil || !isKSpaceSystem(sys) {
		http.Error(w, "unknown system", http.StatusBadRequest)
		return
	}
	key := mapSystemKey{app, sys}
	if e, ok := mapSystemCache.Load(key); ok && time.Since(e.(mapSystemEntry).at) < mapSystemTTL {
		writeMapSystem(w, e.(mapSystemEntry).body)
		return
	}
	now := time.Now().UTC()
	names := app.newKillNames(ctx)
	out := mapSystemData{Kills: []mapSystemKill{}, More: "/intel/kills/?system=" + strconv.FormatInt(sys, 10)}
	// Enough rows for both the latest few and a camp's hour.
	rows, err := app.queries.ListKillsBySystem(ctx, db.ListKillsBySystemParams{SystemID: sys, BeforeTime: now.Add(time.Hour), BeforeID: 1 << 62, MaxRows: 40})
	if err != nil {
		logging.Errorf("map: system %d: kills: %v", sys, err)
	}
	for i, k := range rows {
		if i < mapSystemKills {
			out.Kills = append(out.Kills, mapKillRow(names, k))
		}
	}
	out.Camp = app.mapSystemCamp(ctx, names, sys, now, rows)
	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, "could not encode", http.StatusInternalServerError)
		return
	}
	mapSystemCache.Store(key, mapSystemEntry{at: time.Now(), body: body})
	writeMapSystem(w, body)
}

func writeMapSystem(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=30")
	_, _ = w.Write(body)
}

func mapKillRow(names *killNames, k db.Kill) mapSystemKill {
	row := names.row(k)
	group := row.AllianceName
	if group == "" {
		group = row.CorpName
	}
	return mapSystemKill{ID: row.ID, Ship: row.ShipName, ShipID: row.ShipTypeID, Victim: row.VictimName, Group: group,
		Value: row.Value, At: k.KillmailTime.Unix(), Solo: row.Solo, NPC: row.NPC}
}

// mapSystemCamp finds this system's camp as the threat layer does,
// then reports it from the camp's public kills: the group's pilots and
// ships across them, and the victims. recent is the system's latest
// public kills, newest first.
func (app *Application) mapSystemCamp(ctx context.Context, names *killNames, sys int64, now time.Time, recent []db.Kill) *mapSystemCamp {
	krows, err := app.queries.ListSystemThreatKills(ctx, db.ListSystemThreatKillsParams{SystemID: sys, Since: now.Add(-threatWindow)})
	if err != nil || len(krows) == 0 {
		return nil
	}
	since := now.Add(-threatCampWindow)
	arows, err := app.queries.ListSystemKillAttackers(ctx, db.ListSystemKillAttackersParams{SystemID: sys, Since: since})
	if err != nil {
		logging.Errorf("map: system %d: attackers: %v", sys, err)
		return nil
	}
	types := make([]int64, len(krows))
	for i, r := range krows {
		types[i] = r.ShipTypeID
	}
	structs := app.structureTypes(ctx, types)
	kills := make([]threatKill, len(krows))
	for i, r := range krows {
		kills[i] = threatKill{ID: r.KillmailID, System: r.SystemID, Time: r.KillmailTime, Public: r.Public, VictimCorp: r.VictimCorporationID, Structure: structs[r.ShipTypeID]}
	}
	atk := make([]threatAttacker, len(arows))
	for i, a := range arows {
		atk[i] = threatAttacker{Kill: a.KillmailID, Character: a.CharacterID, Corp: a.CorporationID, Alliance: a.AllianceID}
	}
	th := buildThreat(now, kills, atk, func(kind string, id int64) string {
		switch kind {
		case "alliance":
			return names.alliance(id)
		case "corporation":
			return names.corp(id)
		}
		return names.character(id)
	})
	var c *mapCamp
	for i := range th.Camps {
		if th.Camps[i].System == sys {
			c = &th.Camps[i]
		}
	}
	if c == nil {
		return nil
	}
	camp := &mapSystemCamp{Kind: c.Kind, GroupID: c.GroupID, Group: c.Group, Kills: c.Kills, Since: c.Since,
		Href: "/intel/kills/?" + c.Kind + "=" + strconv.FormatInt(c.GroupID, 10), Ships: []mapSystemShip{}, Victims: []mapSystemKill{}}
	// The group's pilots and ships across the camp's kills.
	member := func(a db.ListSystemKillAttackersRow) bool {
		switch c.Kind {
		case "alliance":
			return a.AllianceID == c.GroupID
		case "corporation":
			return a.CorporationID == c.GroupID
		}
		return a.CharacterID == c.GroupID
	}
	// Composition counts distinct campers: a pilot counts once per
	// ship type they flew in the window, however many of the camp's
	// kills they were on. (Counting involvements instead would call
	// one Loki pilot on sixteen kills "Loki x16".)
	pilots, onKill := map[int64]bool{}, map[int64]bool{}
	shipPilots := map[int64]map[int64]bool{}
	for _, a := range arows {
		if !member(a) {
			continue
		}
		onKill[a.KillmailID] = true
		pilots[a.CharacterID] = true
		if a.ShipTypeID > 0 {
			flying := shipPilots[a.ShipTypeID]
			if flying == nil {
				flying = map[int64]bool{}
				shipPilots[a.ShipTypeID] = flying
			}
			flying[a.CharacterID] = true
		}
	}
	camp.Pilots = len(pilots)
	for id, flying := range shipPilots {
		camp.Ships = append(camp.Ships, mapSystemShip{ID: id, Name: names.typeName(id), N: len(flying)})
	}
	sort.Slice(camp.Ships, func(i, j int) bool {
		if camp.Ships[i].N != camp.Ships[j].N {
			return camp.Ships[i].N > camp.Ships[j].N
		}
		return camp.Ships[i].Name < camp.Ships[j].Name
	})
	if len(camp.Ships) > 6 {
		camp.Ships = camp.Ships[:6]
	}
	for _, k := range recent {
		if onKill[k.KillmailID] && !k.KillmailTime.Before(since) {
			camp.Victims = append(camp.Victims, mapKillRow(names, k))
		}
	}
	if len(camp.Victims) > 8 {
		camp.Victims = camp.Victims[:8]
	}
	return camp
}
