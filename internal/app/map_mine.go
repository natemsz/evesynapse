package app

// ---------------------------------------------------------------------------
// The map's data layers: the reader's own data, by system, for the Data
// menu (map.js). One private payload, /map/mine.json, built from what the
// worker already keeps for each linked character:
//
//   pilots      where each character is (location, ship, online), and
//               how fresh that is
//   fleet       the fleet a character is in; its members and where they
//               are when one of the reader's characters is its boss (ESI
//               shows a fleet's members to its boss only)
//   clones      home stations and jump clones
//   market      sell and buy orders and outstanding contracts
//   industry    industry jobs and planetary colonies
//   structures  the corporation's Upwell structures (a director's view)
//   assets      the estimated ISK of items per system
//   pvp         kills and losses, the last 30 days
//   mining      the mining ledger, the last 30 days
//   posted      the reader's own scans and intel reports
//
// Everything but the fleet is read from stored snapshots; the fleet is
// asked of ESI, as the character, at most once a minute. The payload is
// held per account for a minute too, so the map's refresh is cheap.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	mineCacheFor  = time.Minute
	mineWindow    = 30 * 24 * time.Hour
	minePostedFor = 7 * 24 * time.Hour
)

type minePilot struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Sys      int64  `json:"sys"`
	Online   bool   `json:"online"`
	Ship     string `json:"ship,omitempty"`
	ShipType int64  `json:"shipType,omitempty"`
	ShipName string `json:"shipName,omitempty"`
	Docked   bool   `json:"docked,omitempty"`
	At       int64  `json:"at"` // when the location was read
}

type mineFleetMember struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Sys  int64  `json:"sys"`
	Ship string `json:"ship,omitempty"`
	Mine bool   `json:"mine,omitempty"`
}

type mineFleet struct {
	ID      int64             `json:"id"`
	Boss    bool              `json:"boss"`
	Members []mineFleetMember `json:"members"`
	// In names the reader's characters in the fleet, when the members
	// themselves cannot be read (no character of theirs is boss).
	In []string `json:"in,omitempty"`
}

type mineClone struct {
	Sys      int64  `json:"sys"`
	Home     bool   `json:"home,omitempty"`
	Char     string `json:"char"`
	Name     string `json:"name,omitempty"`
	Implants int    `json:"implants,omitempty"`
}

type mineMarket struct {
	Sys       int64   `json:"sys"`
	Sell      int     `json:"sell,omitempty"`
	Buy       int     `json:"buy,omitempty"`
	ISKSell   float64 `json:"iskSell,omitempty"`
	ISKBuy    float64 `json:"iskBuy,omitempty"`
	Contracts int     `json:"contracts,omitempty"`
}

type mineIndustry struct {
	Sys      int64 `json:"sys"`
	Jobs     int   `json:"jobs,omitempty"`
	Ready    int   `json:"ready,omitempty"`
	NextEnd  int64 `json:"next,omitempty"` // the soonest running job's end
	Colonies int   `json:"colonies,omitempty"`
}

type mineStructure struct {
	Sys   int64  `json:"sys"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	State string `json:"state"`
	Fuel  int64  `json:"fuel,omitempty"`  // when the fuel runs out
	Timer int64  `json:"timer,omitempty"` // a reinforcement timer's end
}

type mineHeat struct {
	Sys int64   `json:"sys"`
	V   float64 `json:"v"`           // the heat value (ISK, kills...)
	A   float64 `json:"a,omitempty"` // a second figure (losses, ISK...)
	B   float64 `json:"b,omitempty"`
	C   float64 `json:"c,omitempty"`
}

type minePosted struct {
	Sys     int64 `json:"sys"`
	Reports int   `json:"reports,omitempty"`
	Scans   int   `json:"scans,omitempty"`
	Last    int64 `json:"last"`
}

// minePayload is /map/mine.json.
type minePayload struct {
	Now        int64           `json:"now"`
	Pilots     []minePilot     `json:"pilots"`
	Fleets     []mineFleet     `json:"fleets"`
	Clones     []mineClone     `json:"clones"`
	Market     []mineMarket    `json:"market"`
	Industry   []mineIndustry  `json:"industry"`
	Structures []mineStructure `json:"structures"`
	Assets     []mineHeat      `json:"assets"`
	PvP        []mineHeat      `json:"pvp"`
	Mining     []mineHeat      `json:"mining"`
	Posted     []minePosted    `json:"posted"`
}

type mineCacheEntry struct {
	at   time.Time
	body []byte
}

var mineCache = struct {
	sync.Mutex
	m map[int64]mineCacheEntry
}{m: map[int64]mineCacheEntry{}}

// handleMapMine is GET /map/mine.json.
func (app *Application) handleMapMine(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	userID := app.userID(ctx)
	if userID == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{}`))
		return
	}
	mineCache.Lock()
	e, ok := mineCache.m[userID]
	mineCache.Unlock()
	if ok && time.Since(e.at) < mineCacheFor && r.URL.Query().Get("fresh") == "" {
		_, _ = w.Write(e.body)
		return
	}
	body, err := json.Marshal(app.buildMine(ctx, userID))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	mineCache.Lock()
	mineCache.m[userID] = mineCacheEntry{at: time.Now(), body: body}
	mineCache.Unlock()
	_, _ = w.Write(body)
}

// snapAt decodes one stored snapshot and says when it was fetched.
func (app *Application) snapAt(ctx context.Context, characterID int64, kind string, out any) (time.Time, bool) {
	s, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: characterID, Kind: kind})
	if err != nil || json.Unmarshal([]byte(s.Payload), out) != nil {
		return time.Time{}, false
	}
	return s.FetchedAt, true
}

func unixOf(rfc string) int64 {
	if t, err := time.Parse(time.RFC3339, rfc); err == nil {
		return t.Unix()
	}
	return 0
}

func (app *Application) buildMine(ctx context.Context, userID int64) minePayload {
	now := time.Now()
	out := minePayload{Now: now.Unix(), Pilots: []minePilot{}, Fleets: []mineFleet{}, Clones: []mineClone{}, Market: []mineMarket{},
		Industry: []mineIndustry{}, Structures: []mineStructure{}, Assets: []mineHeat{}, PvP: []mineHeat{}, Mining: []mineHeat{}, Posted: []minePosted{}}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Warnf("map: characters of user %d: %v", userID, err)
		return out
	}
	names := app.newKillNames(ctx)
	var charIDs []int64
	// Locations to resolve to systems, with what to credit there.
	locWant := map[int64]bool{}
	type pending struct {
		loc  int64
		fill func(sys int64)
	}
	var later []pending
	want := func(loc int64, fill func(int64)) {
		if loc <= 0 {
			return
		}
		locWant[loc] = true
		later = append(later, pending{loc, fill})
	}
	market := map[int64]*mineMarket{}
	marketAt := func(sys int64) *mineMarket {
		if market[sys] == nil {
			market[sys] = &mineMarket{Sys: sys}
		}
		return market[sys]
	}
	industry := map[int64]*mineIndustry{}
	industryAt := func(sys int64) *mineIndustry {
		if industry[sys] == nil {
			industry[sys] = &mineIndustry{Sys: sys}
		}
		return industry[sys]
	}
	assetsISK := map[int64]float64{}
	assetsN := map[int64]float64{}
	// Values in the house order (truevalue.go): The Forge's swept
	// typical sells, then the stored estimated sell; the guide's
	// average prices nothing.
	typicals, trueValues := app.TypicalSells(ctx), app.storedTrueValues(ctx)
	priceOf := func(typeID int64) (float64, bool) {
		return pickValuationPrice(typicals[typeID], estSellOf(trueValues, typeID))
	}
	structSeen := map[int64]bool{}
	mining := map[int64]*mineHeat{}

	for _, ch := range chars {
		charIDs = append(charIDs, ch.CharacterID)
		// Where they are.
		var loc esi.Location
		if at, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapLocation, &loc); ok && loc.SolarSystemID > 0 {
			p := minePilot{ID: ch.CharacterID, Name: ch.Name, Sys: loc.SolarSystemID, At: at.Unix(), Docked: loc.StationID > 0 || loc.StructureID > 0}
			var on esi.Online
			if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapOnline, &on); ok {
				p.Online = on.Online
			}
			var ship esi.Ship
			if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapShip, &ship); ok && ship.ShipTypeID > 0 {
				p.ShipType, p.ShipName, p.Ship = ship.ShipTypeID, ship.ShipName, names.typeName(ship.ShipTypeID)
			}
			out.Pilots = append(out.Pilots, p)
		}
		// Clones.
		var clones esi.Clones
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapClones, &clones); ok {
			name := ch.Name
			want(clones.HomeLocation.LocationID, func(sys int64) {
				out.Clones = append(out.Clones, mineClone{Sys: sys, Home: true, Char: name})
			})
			for _, jc := range clones.JumpClones {
				jc := jc
				want(jc.LocationID, func(sys int64) {
					out.Clones = append(out.Clones, mineClone{Sys: sys, Char: name, Name: jc.Name, Implants: len(jc.Implants)})
				})
			}
		}
		// Market orders and contracts.
		var orders []esi.MarketOrder
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapOrders, &orders); ok {
			for _, o := range orders {
				sys, isk := o.SystemID, o.Price*float64(o.VolumeRemain)
				if sys == 0 {
					continue
				}
				m := marketAt(sys)
				if o.IsBuyOrder {
					m.Buy++
					m.ISKBuy += isk
				} else {
					m.Sell++
					m.ISKSell += isk
				}
			}
		}
		var contracts []esi.Contract
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapContracts, &contracts); ok {
			for _, c := range contracts {
				if c.Status != "outstanding" || c.IssuerID != ch.CharacterID {
					continue
				}
				want(c.StartLocationID, func(sys int64) { marketAt(sys).Contracts++ })
			}
		}
		// Industry jobs and colonies.
		var jobs esi.IndustryJobs
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapIndustryJobs, &jobs); ok {
			for _, j := range jobs {
				if j.Status != "active" && j.Status != "ready" && j.Status != "paused" {
					continue
				}
				j := j
				facility := j.StationID
				if facility == 0 {
					facility = j.FacilityID
				}
				want(facility, func(sys int64) {
					in := industryAt(sys)
					in.Jobs++
					end := unixOf(j.EndDate)
					if j.Status == "ready" || (end > 0 && end <= now.Unix()) {
						in.Ready++
					} else if end > 0 && (in.NextEnd == 0 || end < in.NextEnd) {
						in.NextEnd = end
					}
				})
			}
		}
		var colonies esi.Colonies
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapPlanets, &colonies); ok {
			for _, c := range colonies {
				if c.SolarSystemID > 0 {
					industryAt(c.SolarSystemID).Colonies++
				}
			}
		}
		// The corporation's structures (stored under a director).
		var structures esi.CorpStructures
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapCorpStructures, &structures) {
			for _, s := range structures {
				if structSeen[s.StructureID] || s.SystemID == 0 {
					continue
				}
				structSeen[s.StructureID] = true
				out.Structures = append(out.Structures, mineStructure{Sys: s.SystemID, Name: s.Name, Type: names.typeName(s.TypeID), State: s.State,
					Fuel: unixOf(s.FuelExpires), Timer: unixOf(s.StateTimerEnd)})
			}
		}
		// Assets: each item's value at the top of its container chain.
		var assets []esi.Asset
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapAssets, &assets); ok {
			byItem := make(map[int64]esi.Asset, len(assets))
			for _, a := range assets {
				byItem[a.ItemID] = a
			}
			roots := map[int64]float64{}
			for _, a := range assets {
				price, ok := priceOf(a.TypeID)
				if !ok || price <= 0 || a.IsBlueprintCopy {
					continue
				}
				top := a
				for i := 0; i < 8 && top.LocationType == "item"; i++ {
					parent, ok := byItem[top.LocationID]
					if !ok {
						break
					}
					top = parent
				}
				v := price * float64(max(a.Quantity, 1))
				if top.LocationType == "solar_system" {
					assetsISK[top.LocationID] += v
					assetsN[top.LocationID]++
					continue
				}
				roots[top.LocationID] += v
			}
			for loc, v := range roots {
				v := v
				want(loc, func(sys int64) { assetsISK[sys] += v; assetsN[sys]++ })
			}
		}
		// Mining, the last 30 days.
		var ledger esi.MiningLedger
		if _, ok := app.snapAt(ctx, ch.CharacterID, esi.SnapMining, &ledger); ok {
			cut := now.Add(-mineWindow).Format("2006-01-02")
			for _, e := range ledger {
				if e.Date < cut || e.SolarSystemID == 0 {
					continue
				}
				h := mining[e.SolarSystemID]
				if h == nil {
					h = &mineHeat{Sys: e.SolarSystemID}
					mining[e.SolarSystemID] = h
				}
				// The heat is what was mined (every unit counts, priced
				// or not); its ISK, where known, rides along.
				h.V += float64(e.Quantity)
				if price, ok := priceOf(e.TypeID); ok {
					h.A += price * float64(e.Quantity)
				}
			}
		}
	}

	// Resolve the locations gathered above, in one read.
	if len(locWant) > 0 {
		ids := make([]int64, 0, len(locWant))
		for id := range locWant {
			ids = append(ids, id)
		}
		sysOf := map[int64]int64{}
		if rows, err := app.queries.ListLocationSystems(ctx, ids); err == nil {
			for _, r := range rows {
				sysOf[r.LocationID] = r.SystemID
			}
		}
		for _, p := range later {
			if sys := sysOf[p.loc]; sys > 0 {
				p.fill(sys)
			}
		}
	}
	for _, m := range market {
		out.Market = append(out.Market, *m)
	}
	for _, in := range industry {
		out.Industry = append(out.Industry, *in)
	}
	for sys, v := range assetsISK {
		out.Assets = append(out.Assets, mineHeat{Sys: sys, V: v, A: assetsN[sys]})
	}
	for _, h := range mining {
		out.Mining = append(out.Mining, *h)
	}
	if len(charIDs) > 0 {
		if rows, err := app.queries.ListAccountKillsBySystem(ctx, db.ListAccountKillsBySystemParams{CharacterIds: charIDs, Since: now.Add(-mineWindow)}); err == nil {
			for _, r := range rows {
				out.PvP = append(out.PvP, mineHeat{Sys: r.SystemID, V: float64(r.Kills + r.Losses), A: float64(r.Kills), B: float64(r.Losses), C: r.IskKilled + r.IskLost})
			}
		} else {
			logging.Warnf("map: kills by system: %v", err)
		}
	}
	posted := map[int64]*minePosted{}
	postedAt := func(sys int64) *minePosted {
		if posted[sys] == nil {
			posted[sys] = &minePosted{Sys: sys}
		}
		return posted[sys]
	}
	if rows, err := app.queries.ListAuthoredIntelBySystem(ctx, db.ListAuthoredIntelBySystemParams{UserID: userID, Since: now.Add(-minePostedFor)}); err == nil {
		for _, r := range rows {
			p := postedAt(r.SystemID)
			p.Reports = int(r.Reports)
			p.Last = max(p.Last, r.LastSeen.Unix())
		}
	}
	if rows, err := app.queries.ListAccountScanSystems(ctx, userID); err == nil {
		for _, r := range rows {
			p := postedAt(r.SystemID)
			p.Scans++
			p.Last = max(p.Last, r.CreatedAt.Unix())
		}
	}
	for _, p := range posted {
		out.Posted = append(out.Posted, *p)
	}
	if f := app.mineFleets(ctx, chars); f != nil {
		out.Fleets = f
	}
	sort.Slice(out.Pilots, func(i, j int) bool { return out.Pilots[i].Name < out.Pilots[j].Name })
	return out
}

// --- The fleet: live, as the character, at most once a minute ---

type fleetCacheEntry struct {
	at    time.Time
	fleet *mineFleet
}

var fleetCache = struct {
	sync.Mutex
	m map[int64]fleetCacheEntry // by character
}{m: map[int64]fleetCacheEntry{}}

// mineFleets reads the fleets the account's characters are in. A fleet
// whose boss is one of them is read whole: members, ships, systems.
func (app *Application) mineFleets(ctx context.Context, chars []db.Character) []mineFleet {
	byID := map[int64]*mineFleet{}
	mine := map[int64]bool{}
	for _, ch := range chars {
		mine[ch.CharacterID] = true
	}
	var order []int64
	for _, ch := range chars {
		if !module.CharacterHasScope(ch, fleetScope) {
			continue
		}
		f := app.characterFleet(ctx, ch)
		if f == nil {
			continue
		}
		have := byID[f.ID]
		if have == nil {
			cp := *f
			cp.In = nil
			byID[f.ID] = &cp
			order = append(order, f.ID)
			have = byID[f.ID]
		}
		if f.Boss && !have.Boss {
			have.Boss, have.Members = true, f.Members
		}
		have.In = append(have.In, ch.Name)
	}
	var out []mineFleet
	for _, id := range order {
		f := byID[id]
		for i := range f.Members {
			f.Members[i].Mine = mine[f.Members[i].ID]
		}
		out = append(out, *f)
	}
	return out
}

func (app *Application) characterFleet(ctx context.Context, ch db.Character) *mineFleet {
	fleetCache.Lock()
	e, ok := fleetCache.m[ch.CharacterID]
	fleetCache.Unlock()
	if ok && time.Since(e.at) < mineCacheFor {
		return e.fleet
	}
	keep := func(f *mineFleet) *mineFleet {
		fleetCache.Lock()
		fleetCache.m[ch.CharacterID] = fleetCacheEntry{at: time.Now(), fleet: f}
		fleetCache.Unlock()
		return f
	}
	token, err := app.AccessToken(ctx, ch)
	if err != nil {
		return keep(nil)
	}
	var membership struct {
		FleetID int64  `json:"fleet_id"`
		Role    string `json:"role"`
		BossID  int64  `json:"fleet_boss_id"`
	}
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/characters/%d/fleet/", ch.CharacterID), &membership); err != nil {
		var se *esi.StatusError
		if !errors.As(err, &se) || se.Code != http.StatusNotFound {
			logging.Debugf("map: fleet of %d: %v", ch.CharacterID, err)
		}
		return keep(nil)
	}
	f := &mineFleet{ID: membership.FleetID}
	if membership.BossID != ch.CharacterID {
		return keep(f)
	}
	var members []struct {
		CharacterID   int64 `json:"character_id"`
		ShipTypeID    int64 `json:"ship_type_id"`
		SolarSystemID int64 `json:"solar_system_id"`
	}
	if err := app.esi.Get(ctx, token, fmt.Sprintf("/fleets/%d/members/", membership.FleetID), &members); err != nil {
		logging.Debugf("map: fleet %d members: %v", membership.FleetID, err)
		return keep(f)
	}
	f.Boss = true
	names := app.newKillNames(ctx)
	var unknown []int64
	for _, m := range members {
		if _, ok := app.esi.CachedCharacterName(m.CharacterID); !ok {
			unknown = append(unknown, m.CharacterID)
		}
	}
	if len(unknown) > 0 {
		_, _ = app.resolveUniverseNamesLimited(ctx, &fetchBudget{left: 2}, unknown)
	}
	for _, m := range members {
		f.Members = append(f.Members, mineFleetMember{ID: m.CharacterID, Name: names.character(m.CharacterID), Sys: m.SolarSystemID, Ship: names.typeName(m.ShipTypeID)})
	}
	return keep(f)
}
