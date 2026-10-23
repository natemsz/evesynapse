package app

// ---------------------------------------------------------------------------
// Scans (Intel): paste a directional scan, a local member list, a fleet
// composition or probe scanner results at /intel/scan/, get a reading
// at an unlisted link /intel/scan/{id}/.
//
// internal/scan reads the paste; this file enriches it once, at paste
// time, and stores the reading (schema 077): d-scan types get their
// ship class and category from the SDE and the scan its system from
// the celestials on it; a local list is resolved to pilots, their
// corporations and alliances, and each pilot's recent public kills on
// our killboard. A stored reading never changes: a local scan says who
// was where then.
//
// Scans are unlisted. Nobody can list them but who pasted them: by
// account when signed in, by session otherwise (the session keeps the
// ids it pasted). The map's system card offers the reader's own scans
// of that system (/map/scans.json) and a link to paste one.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
)

const (
	scanRetention = 90 * 24 * time.Hour
	// scanGridKm is how near a d-scan item must be to count as on
	// grid with the scanner.
	scanGridKm = 10000
	// scanKillWindow is how far back a local pilot's kills count.
	scanKillWindow = 30 * 24 * time.Hour
	scanSessionKey = "scans"
	scanOwnMax     = 30
)

// SDE categories a d-scan sorts by.
const (
	catCelestial  = 2
	catStation    = 3
	catShip       = 6
	catCharge     = 8
	catEntity     = 11
	catDrone      = 18
	catDeployable = 22
	catStarbase   = 23
	catSov        = 40
	catOrbitals   = 46
	catStructure  = 65
	catFighter    = 87
)

// scanNoiseGroups are the SDE groups a reading leaves out entirely:
// billboards and large collidables are scenery on every scan, and
// counting them pads every total on the page.
var scanNoiseGroups = map[string]bool{
	"Billboard": true, "Large Collidable Object": true,
	"Large Collidable Structure": true,
}

// objectKindByGroup labels the SDE groups a reader wants told apart
// from their category's plain members: wrecks, stargates and
// containers share Celestial with the planets. Anything in the
// Celestial or Entity category whose group is not here reads as a
// celestial (planets, moons, suns) or an NPC.
var objectKindByGroup = map[string]string{
	"Stargate": "Stargate", "Wreck": "Wreck",
	"Cargo Container": "Container", "Secure Cargo Container": "Container",
	"Freight Container": "Container", "Audit Log Secure Container": "Container",
	"Spawn Container": "Container", "Mission Container": "Container",
	"Scatter Container": "Container", "Irregular Container": "Container",
}

// shipHullClasses folds the SDE's ship groups into the hull
// classes a fleet read is given in.
var shipHullClasses = map[string]string{
	"Titan": "Capitals", "Dreadnought": "Capitals", "Lancer Dreadnought": "Capitals",
	"Carrier": "Capitals", "Supercarrier": "Capitals", "Force Auxiliary": "Capitals",
	"Capital Industrial Ship": "Capitals", "Command Carrier": "Capitals",
	"Freighter": "Capitals", "Jump Freighter": "Capitals",
	"Battleship": "Battleships", "Elite Battleship": "Battleships",
	"Marauder": "Battleships", "Black Ops": "Battleships",
	"Combat Battlecruiser": "Battlecruisers", "Attack Battlecruiser": "Battlecruisers",
	"Command Ship": "Battlecruisers",
	"Cruiser":      "Cruisers", "Heavy Assault Cruiser": "Cruisers",
	"Heavy Interdiction Cruiser": "Cruisers", "Logistics": "Cruisers",
	"Force Recon Ship": "Cruisers", "Combat Recon Ship": "Cruisers",
	"Strategic Cruiser": "Cruisers", "Flag Cruiser": "Cruisers",
	"Special Edition Yachts": "Cruisers",
	"Destroyer":              "Destroyers", "Tactical Destroyer": "Destroyers",
	"Interdictor": "Destroyers", "Command Destroyer": "Destroyers",
	"Frigate": "Frigates", "Assault Frigate": "Frigates", "Covert Ops": "Frigates",
	"Interceptor": "Frigates", "Stealth Bomber": "Frigates",
	"Electronic Attack Ship": "Frigates", "Logistics Frigate": "Frigates",
	"Expedition Frigate": "Frigates", "Prototype Exploration Ship": "Frigates",
	"Corvette": "Frigates",
	"Hauler":   "Industrial", "Deep Space Transport": "Industrial",
	"Blockade Runner": "Industrial", "Mining Barge": "Industrial",
	"Exhumer": "Industrial", "Industrial Command Ship": "Industrial",
	"Expedition Command Ship": "Industrial",
	"Capsule":                 "Pods",
}

// shipRoleGroups gives a ship group's fleet role. Groups not here
// fight: their role is DPS.
var shipRoleGroups = map[string]string{
	"Logistics": "Logistics", "Logistics Frigate": "Logistics",
	"Force Auxiliary": "Logistics",
	"Command Ship":    "Command", "Command Destroyer": "Command",
	"Industrial Command Ship": "Command", "Expedition Command Ship": "Command",
	"Command Carrier": "Command", "Flag Cruiser": "Command",
	"Interdictor": "Interdiction", "Heavy Interdiction Cruiser": "Interdiction",
	"Force Recon Ship": "Recon & EWAR", "Combat Recon Ship": "Recon & EWAR",
	"Electronic Attack Ship": "Recon & EWAR", "Covert Ops": "Recon & EWAR",
	"Stealth Bomber": "Bombers",
	"Capsule":        "Non-combat", "Shuttle": "Non-combat", "Corvette": "Non-combat",
	"Citizen Ships": "Non-combat", "Hauler": "Non-combat",
	"Deep Space Transport": "Non-combat", "Blockade Runner": "Non-combat",
	"Mining Barge": "Non-combat", "Exhumer": "Non-combat",
	"Expedition Frigate": "Non-combat", "Prototype Exploration Ship": "Non-combat",
	"Freighter": "Non-combat", "Jump Freighter": "Non-combat",
	"Capital Industrial Ship": "Non-combat", "Special Edition Yachts": "Non-combat",
}

// shipRoleTypes overrides the group role hull by hull for the
// mixed T1 groups: the T1 logistics and EWAR frigates and cruisers
// sit in the plain Frigate and Cruiser groups beside the damage
// hulls.
var shipRoleTypes = map[int64]string{
	// T1 logistics cruisers.
	620: "Logistics", 625: "Logistics", 631: "Logistics", 634: "Logistics",
	// T1 logistics frigates.
	582: "Logistics", 590: "Logistics", 592: "Logistics", 599: "Logistics",
	// T1 EWAR cruisers.
	628: "Recon & EWAR", 630: "Recon & EWAR", 632: "Recon & EWAR", 633: "Recon & EWAR",
	// T1 EWAR frigates.
	584: "Recon & EWAR", 609: "Recon & EWAR", 2161: "Recon & EWAR", 3766: "Recon & EWAR",
}

// notableGroups are the ship groups called out on sight: capital
// hulls and the freighters that feed them.
var notableGroups = map[string]bool{
	"Titan": true, "Dreadnought": true, "Lancer Dreadnought": true,
	"Carrier": true, "Supercarrier": true, "Force Auxiliary": true,
	"Capital Industrial Ship": true, "Command Carrier": true,
	"Freighter": true, "Jump Freighter": true,
}

// specialHulls are alliance-tournament and special-edition ship
// types, by SDE type id: hulls that exist in handfuls.
var specialHulls = map[int64]bool{
	2834: true, 2836: true, 3516: true, 3518: true, 635: true,
	11011: true, 11940: true, 11942: true, 29266: true, 32207: true,
	32209: true, 32788: true, 32790: true, 33395: true, 33397: true,
	33513: true, 33553: true, 33673: true, 33675: true, 34590: true,
	35779: true, 35781: true, 42245: true, 42246: true, 48635: true,
	48636: true, 60764: true, 60765: true, 74141: true, 77726: true,
}

// --- The stored reading ---

// scanResult is a scan's reading, stored as JSON beside the paste.
type scanResult struct {
	Kind     scan.Kind     `json:"kind"`
	SystemID int64         `json:"system,omitempty"`
	Skipped  int           `json:"skipped,omitempty"`
	DScan    *dscanReading `json:"dscan,omitempty"`
	Local    *localReading `json:"local,omitempty"`
	Fleet    *fleetReading `json:"fleet,omitempty"`
	Probe    *probeReading `json:"probe,omitempty"`
}

// scanCount is one tally: a ship class, a type, a system, a wing.
type scanCount struct {
	ID    int64  `json:"id,omitempty"`
	Name  string `json:"name"`
	Class string `json:"class,omitempty"`
	N     int    `json:"n"`
	Grid  int    `json:"grid,omitempty"`
	// Pct is the bar width on the page: N against the list's most.
	Pct int `json:"-"`
}

// withPct sets each count's bar width against the list's largest.
func withPct(list []scanCount) []scanCount {
	top := 1
	for _, c := range list {
		top = max(top, c.N)
	}
	for i := range list {
		list[i].Pct = max(2, list[i].N*100/top)
	}
	return list
}

type dscanReading struct {
	Items     int `json:"items"`
	Ships     int `json:"ships"`
	ShipsGrid int `json:"ships_grid"`
	// ShipsOff is Ships - ShipsGrid, filled for the page.
	ShipsOff int         `json:"-"`
	Classes  []scanCount `json:"classes"`
	Types    []scanCount `json:"types"`
	// Hulls and Roles are the composition rollups of Types, by
	// hull class and by fleet role.
	Hulls []scanCount `json:"hulls,omitempty"`
	Roles []scanCount `json:"roles,omitempty"`
	// Notable is the callout list: capital and special hulls on
	// the scan, and massed cheap hulls.
	Notable      []scanCount `json:"notable,omitempty"`
	Structures   []scanCount `json:"structures,omitempty"`
	Deployables  []scanCount `json:"deployables,omitempty"`
	Drones       int         `json:"drones,omitempty"`
	Fighters     int         `json:"fighters,omitempty"`
	DroneTypes   []scanCount `json:"drone_types,omitempty"`
	FighterTypes []scanCount `json:"fighter_types,omitempty"`
	// Objects is everything else a d-scan shows, itemized by
	// type: wrecks, stargates, containers, charges, orbitals,
	// NPCs. Class carries the kind.
	Objects    []scanCount `json:"objects,omitempty"`
	Celestials int         `json:"celestials,omitempty"`
}

type localPilot struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	CorpID     int64     `json:"corp"`
	AllianceID int64     `json:"alliance,omitempty"`
	Kills      int64     `json:"kills,omitempty"`
	LastKill   time.Time `json:"last_kill,omitempty"`
	LastSystem int64     `json:"last_system,omitempty"`
}

type localGroup struct {
	ID     int64        `json:"id"`
	Name   string       `json:"name"`
	Ticker string       `json:"ticker,omitempty"`
	N      int          `json:"n"`
	Kills  int64        `json:"kills,omitempty"`
	Corps  []localGroup `json:"corps,omitempty"`
	Pct    int          `json:"-"`
	// Corp marks a corporation standing outside any alliance.
	Corp bool `json:"corp,omitempty"`
	// Pilots is the group's roster as the page shows it, grouped
	// from the reading's pilot list at render (a reading stored
	// before rosters were shown carries the same pilots at the
	// top level); MorePilots counts any past the rendered cap.
	Pilots     []localPilot `json:"pilots,omitempty"`
	MorePilots int          `json:"-"`
}

type localReading struct {
	Pilots    []localPilot `json:"pilots"`
	Alliances []localGroup `json:"alliances"`
	Unknown   []string     `json:"unknown,omitempty"`
	// Partial is set when ESI could not resolve everything.
	Partial bool `json:"partial,omitempty"`
}

type fleetReading struct {
	Members []scan.FleetRow `json:"members"`
	Classes []scanCount     `json:"classes"`
	Types   []scanCount     `json:"types"`
	Systems []scanCount     `json:"systems"`
	Wings   []scanCount     `json:"wings"`
}

type probeReading struct {
	Sigs   []scan.ProbeRow `json:"sigs"`
	Groups []scanCount     `json:"groups"`
	// Unresolved counts signatures not yet scanned to 100%.
	Unresolved int `json:"unresolved"`
}

// --- Reading a paste ---

// tally counts names, keeping the first-seen ID and class.
type tally struct {
	order []string
	at    map[string]*scanCount
}

func newTally() *tally { return &tally{at: map[string]*scanCount{}} }

func (t *tally) add(id int64, name, class string, grid bool) {
	c := t.at[name]
	if c == nil {
		c = &scanCount{ID: id, Name: name, Class: class}
		t.at[name] = c
		t.order = append(t.order, name)
	}
	c.N++
	if grid {
		c.Grid++
	}
}

// sorted lists the tallies, most first, then by name.
func (t *tally) sorted() []scanCount {
	out := make([]scanCount, 0, len(t.order))
	for _, k := range t.order {
		out = append(out, *t.at[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// readScan enriches a parsed paste. systemID, when the reader named
// one, wins over any system a d-scan's celestials suggest.
func (app *Application) readScan(ctx context.Context, p scan.Result, systemID int64) scanResult {
	res := scanResult{Kind: p.Kind, SystemID: systemID, Skipped: p.Skipped}
	switch p.Kind {
	case scan.KindDScan:
		var detected int64
		res.DScan, detected = app.readDScan(ctx, p.DScan)
		if res.SystemID == 0 {
			res.SystemID = detected
		}
	case scan.KindLocal:
		res.Local = app.readLocal(ctx, p.Local)
	case scan.KindFleet:
		res.Fleet = readFleet(p.Fleet)
	case scan.KindProbe:
		res.Probe = readProbe(p.Probe)
	}
	return res
}

// readDScan sorts a d-scan by category and ship class, and reads
// the system off its celestials' names.
func (app *Application) readDScan(ctx context.Context, rows []scan.DScanRow) (*dscanReading, int64) {
	ids := make([]int64, 0, len(rows))
	seen := map[int64]bool{}
	for _, r := range rows {
		if !seen[r.TypeID] {
			seen[r.TypeID] = true
			ids = append(ids, r.TypeID)
		}
	}
	class, cat := map[int64]string{}, map[int64]int64{}
	if gs, err := app.queries.ListTypeGroupNames(ctx, ids); err == nil {
		for _, g := range gs {
			class[g.TypeID] = g.Name
		}
	}
	if cs, err := app.queries.ListSDETypeCategories(ctx, ids); err == nil {
		for _, c := range cs {
			cat[c.TypeID] = c.CategoryID
		}
	}
	d := &dscanReading{Items: len(rows)}
	classes, types, structs, deploy := newTally(), newTally(), newTally(), newTally()
	droneTypes, fighterTypes, objects := newTally(), newTally(), newTally()
	// The system is voted on with weights: a sun names its system
	// outright, planets, moons, belts and NPC stations are strong
	// evidence, player structures and gates are weak (a system
	// full of citadels named for it must not outvote its own sun),
	// and stargates never vote -- a gate's name is its destination.
	sysVotes := map[string]int{}
	for _, r := range rows {
		grid := r.DistKm >= 0 && r.DistKm <= scanGridKm
		gname := class[r.TypeID]
		if scanNoiseGroups[gname] {
			d.Items--
			continue
		}
		switch c := cat[r.TypeID]; c {
		case catShip:
			d.Ships++
			if grid {
				d.ShipsGrid++
			}
			cl := gname
			if cl == "" {
				cl = "Ship"
			}
			classes.add(0, cl, "", grid)
			types.add(r.TypeID, r.TypeName, cl, grid)
		case catStructure, catStarbase, catStation, catSov:
			structs.add(r.TypeID, r.TypeName, gname, grid)
			// Stations carry their moon's name; player structures
			// are named "System - Name".
			w := 1
			if c == catStation {
				w = 3
			}
			if s := scan.SystemFromCelestial(r.Name); s != "" {
				sysVotes[s] += w
			} else if i := strings.Index(r.Name, " - "); i > 0 {
				sysVotes[r.Name[:i]] += w
			}
		case catDeployable:
			deploy.add(r.TypeID, r.TypeName, gname, grid)
		case catDrone:
			d.Drones++
			droneTypes.add(r.TypeID, r.TypeName, gname, grid)
		case catFighter:
			d.Fighters++
			fighterTypes.add(r.TypeID, r.TypeName, gname, grid)
		case catCelestial:
			if kind, ok := objectKindByGroup[gname]; ok {
				objects.add(r.TypeID, r.TypeName, kind, grid)
				break
			}
			d.Celestials++
			if s := scan.SystemFromCelestial(r.Name); s != "" {
				w := 3
				if gname == "Sun" {
					w = 10
				}
				sysVotes[s] += w
			}
		case catCharge:
			objects.add(r.TypeID, r.TypeName, "Charge", grid)
		case catEntity:
			kind := objectKindByGroup[gname]
			if kind == "" {
				kind = "NPC"
			}
			objects.add(r.TypeID, r.TypeName, kind, grid)
		case catOrbitals:
			objects.add(r.TypeID, r.TypeName, "Orbital", grid)
		default:
			objects.add(r.TypeID, r.TypeName, "Other", grid)
		}
	}
	d.Classes, d.Types, d.Structures, d.Deployables = classes.sorted(), types.sorted(), structs.sorted(), deploy.sorted()
	d.DroneTypes, d.FighterTypes, d.Objects = droneTypes.sorted(), fighterTypes.sorted(), objects.sorted()
	d.Hulls = rollupCounts(d.Types, func(c scanCount) string {
		if h, ok := shipHullClasses[c.Class]; ok {
			return h
		}
		return "Other"
	})
	d.Roles = rollupCounts(d.Types, func(c scanCount) string {
		if r, ok := shipRoleTypes[c.ID]; ok {
			return r
		}
		if r, ok := shipRoleGroups[c.Class]; ok {
			return r
		}
		return "DPS"
	})
	d.Notable = notableCallouts(d)
	// The system: the name the weighted vote carries, if the SDE
	// knows it (a few lookups at most).
	cands := make([]string, 0, len(sysVotes))
	for s := range sysVotes {
		cands = append(cands, s)
	}
	sort.Slice(cands, func(i, j int) bool {
		if sysVotes[cands[i]] != sysVotes[cands[j]] {
			return sysVotes[cands[i]] > sysVotes[cands[j]]
		}
		return cands[i] < cands[j]
	})
	for i, s := range cands {
		if i == 6 {
			break
		}
		if sys, err := app.queries.GetSDESystemByName(ctx, s); err == nil {
			return d, sys.SystemID
		}
	}
	return d, 0
}

// rollupCounts folds type tallies into coarser buckets by a key
// function, summing counts and on-grid counts.
func rollupCounts(types []scanCount, pick func(scanCount) string) []scanCount {
	t := newTally()
	for _, c := range types {
		k := pick(c)
		e := t.at[k]
		if e == nil {
			e = &scanCount{Name: k}
			t.at[k] = e
			t.order = append(t.order, k)
		}
		e.N += c.N
		e.Grid += c.Grid
	}
	return t.sorted()
}

// notableCallouts picks what a reader looks for first on a scan:
// capital and special hulls, named a type at a time, and cheap
// hulls massed past the point they are the fleet (25 or more of a
// hull class, a fifth of the ships or better).
func notableCallouts(d *dscanReading) []scanCount {
	var out []scanCount
	for _, c := range d.Types {
		switch {
		case notableGroups[c.Class]:
			out = append(out, scanCount{ID: c.ID, Name: c.Name, Class: c.Class, N: c.N, Grid: c.Grid})
		case specialHulls[c.ID]:
			out = append(out, scanCount{ID: c.ID, Name: c.Name, Class: "Special edition", N: c.N, Grid: c.Grid})
		}
	}
	for _, h := range d.Hulls {
		switch h.Name {
		case "Frigates", "Destroyers", "Cruisers", "Battlecruisers", "Battleships", "Industrial":
			if h.N >= 25 && h.N*5 >= d.Ships {
				out = append(out, scanCount{Name: h.Name, Class: "Massed hulls", N: h.N, Grid: h.Grid})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// readLocal resolves a local list: names to pilots, pilots to their
// corporations and alliances, and each pilot's recent public kills.
func (app *Application) readLocal(ctx context.Context, names []string) *localReading {
	l := &localReading{}
	budget := &fetchBudget{left: 12}
	byName := map[string]int64{}
	for start := 0; start < len(names); start += 500 {
		end := min(start+500, len(names))
		if !budget.take() {
			l.Partial = true
			break
		}
		var ids struct {
			Characters []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"characters"`
		}
		if err := app.esi.PostJSON(ctx, "/universe/ids/", names[start:end], &ids); err != nil {
			logging.Warnf("scans: resolve %d local names: %v", end-start, err)
			l.Partial = true
			continue
		}
		for _, c := range ids.Characters {
			byName[strings.ToLower(c.Name)] = c.ID
			app.esi.StoreCharacterName(c.ID, c.Name)
		}
	}
	var charIDs []int64
	for _, n := range names {
		id, ok := byName[strings.ToLower(n)]
		if !ok {
			l.Unknown = append(l.Unknown, n)
			continue
		}
		charIDs = append(charIDs, id)
		l.Pilots = append(l.Pilots, localPilot{ID: id, Name: n})
	}
	aff := map[int64][2]int64{}
	for start := 0; start < len(charIDs); start += 1000 {
		end := min(start+1000, len(charIDs))
		if !budget.take() {
			l.Partial = true
			break
		}
		var rows []struct {
			CharacterID   int64 `json:"character_id"`
			CorporationID int64 `json:"corporation_id"`
			AllianceID    int64 `json:"alliance_id"`
		}
		if err := app.esi.PostJSON(ctx, "/characters/affiliation/", charIDs[start:end], &rows); err != nil {
			logging.Warnf("scans: affiliation of %d pilots: %v", end-start, err)
			l.Partial = true
			continue
		}
		for _, r := range rows {
			aff[r.CharacterID] = [2]int64{r.CorporationID, r.AllianceID}
		}
	}
	activity := map[int64]db.ListPilotKillActivityRow{}
	if len(charIDs) > 0 {
		if rows, err := app.queries.ListPilotKillActivity(ctx, db.ListPilotKillActivityParams{CharacterIds: charIDs, Since: time.Now().Add(-scanKillWindow)}); err == nil {
			for _, r := range rows {
				activity[r.CharacterID] = r
			}
		} else {
			logging.Warnf("scans: pilot kill activity: %v", err)
		}
	}
	// Names for every corporation and alliance: the caches first,
	// one bulk call for the rest.
	var orgIDs []int64
	orgSeen := map[int64]bool{}
	for i := range l.Pilots {
		p := &l.Pilots[i]
		a := aff[p.ID]
		p.CorpID, p.AllianceID = a[0], a[1]
		if act, ok := activity[p.ID]; ok {
			p.Kills, p.LastKill, p.LastSystem = act.Kills, act.LastKill, act.LastSystem
		}
		for _, id := range []int64{p.CorpID, p.AllianceID} {
			if id > 0 && !orgSeen[id] {
				orgSeen[id] = true
				orgIDs = append(orgIDs, id)
			}
		}
	}
	orgName := map[int64]string{}
	var missing []int64
	for _, id := range orgIDs {
		if n, ok := app.esi.CachedCorpName(id); ok && n != "" {
			orgName[id] = n
		} else if n, ok := app.esi.CachedAllianceName(id); ok && n != "" {
			orgName[id] = n
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		got, _ := app.resolveUniverseNamesLimited(ctx, budget, missing)
		for id, n := range got {
			orgName[id] = n
		}
	}
	l.Alliances = groupLocal(l.Pilots, orgName)
	app.fillLocalTickers(ctx, l.Alliances)
	return l
}

// scanTickerBudget bounds the public calls a local paste spends on
// alliance and corporation tickers.
const scanTickerBudget = 30

// fillLocalTickers resolves group tickers from ESI's public org
// records, biggest groups first, under a fixed call budget. A
// group the budget or ESI does not reach keeps no ticker; the
// reading is not marked partial over dressing.
func (app *Application) fillLocalTickers(ctx context.Context, groups []localGroup) {
	type target struct {
		g    *localGroup
		path string
	}
	var targets []target
	for i := range groups {
		g := &groups[i]
		if g.ID <= 0 {
			continue
		}
		kind := "alliances"
		if g.Corp {
			kind = "corporations"
		}
		targets = append(targets, target{g, fmt.Sprintf("/%s/%d/", kind, g.ID)})
	}
	for i := range groups {
		for j := range groups[i].Corps {
			c := &groups[i].Corps[j]
			if c.ID > 0 {
				targets = append(targets, target{c, fmt.Sprintf("/corporations/%d/", c.ID)})
			}
		}
	}
	left := scanTickerBudget
	for _, t := range targets {
		if left == 0 {
			break
		}
		left--
		var rec struct {
			Ticker string `json:"ticker"`
		}
		if err := app.esi.Get(ctx, "", t.path, &rec); err == nil {
			t.g.Ticker = rec.Ticker
		}
	}
}

// groupLocal folds pilots into alliances (corporations without one
// standing as their own group), each with its corporations, biggest
// first.
func groupLocal(pilots []localPilot, orgName map[int64]string) []localGroup {
	type key struct{ alliance, corp int64 }
	groups := map[int64]*localGroup{}
	corps := map[key]*localGroup{}
	var order []int64
	for _, p := range pilots {
		gid := p.AllianceID
		if gid == 0 {
			gid = -p.CorpID // a corporation outside any alliance is its own group
		}
		g := groups[gid]
		if g == nil {
			name := orgName[p.AllianceID]
			id := p.AllianceID
			if p.AllianceID == 0 {
				name, id = orgName[p.CorpID], p.CorpID
			}
			if name == "" {
				name = "Unknown"
			}
			g = &localGroup{ID: id, Name: name, Corp: p.AllianceID == 0}
			groups[gid] = g
			order = append(order, gid)
		}
		g.N++
		g.Kills += p.Kills
		if p.AllianceID == 0 {
			continue
		}
		k := key{p.AllianceID, p.CorpID}
		c := corps[k]
		if c == nil {
			name := orgName[p.CorpID]
			if name == "" {
				name = "Unknown"
			}
			c = &localGroup{ID: p.CorpID, Name: name}
			corps[k] = c
		}
		c.N++
		c.Kills += p.Kills
	}
	for k, c := range corps {
		g := groups[k.alliance]
		g.Corps = append(g.Corps, *c)
	}
	out := make([]localGroup, 0, len(order))
	for _, gid := range order {
		g := groups[gid]
		sort.Slice(g.Corps, func(i, j int) bool {
			if g.Corps[i].N != g.Corps[j].N {
				return g.Corps[i].N > g.Corps[j].N
			}
			return g.Corps[i].Name < g.Corps[j].Name
		})
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func readFleet(rows []scan.FleetRow) *fleetReading {
	f := &fleetReading{Members: rows}
	classes, types, systems, wings := newTally(), newTally(), newTally(), newTally()
	for _, m := range rows {
		classes.add(0, orUnknown(m.Class), "", false)
		types.add(0, orUnknown(m.Ship), m.Class, false)
		systems.add(0, orUnknown(m.System), "", false)
		if m.Wing != "" {
			w := m.Wing
			if i := strings.Index(w, " / "); i > 0 {
				w = w[:i]
			}
			wings.add(0, w, "", false)
		}
	}
	f.Classes, f.Types, f.Systems, f.Wings = classes.sorted(), types.sorted(), systems.sorted(), wings.sorted()
	return f
}

func readProbe(rows []scan.ProbeRow) *probeReading {
	p := &probeReading{Sigs: rows}
	groups := newTally()
	for _, s := range rows {
		t := s.Type
		if t == "" {
			t = "Unscanned"
		}
		if s.Signal < 100 {
			p.Unresolved++
		}
		groups.add(0, t, "", false)
	}
	p.Groups = groups.sorted()
	sort.SliceStable(p.Sigs, func(i, j int) bool {
		a, b := p.Sigs[i], p.Sigs[j]
		if (a.Type == "") != (b.Type == "") {
			return a.Type != ""
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})
	return p
}

func orUnknown(s string) string {
	if s == "" {
		return "Unknown"
	}
	return s
}

// scanTitle is a scan's one-line label: kind and its headline count.
func scanTitle(res scanResult) string {
	switch res.Kind {
	case scan.KindDScan:
		if res.DScan != nil {
			return "D-scan · " + plural(res.DScan.Ships, "ship")
		}
	case scan.KindLocal:
		if res.Local != nil {
			return "Local · " + plural(len(res.Local.Pilots)+len(res.Local.Unknown), "pilot")
		}
	case scan.KindFleet:
		if res.Fleet != nil {
			return "Fleet · " + plural(len(res.Fleet.Members), "member")
		}
	case scan.KindProbe:
		if res.Probe != nil {
			return "Probe scan · " + plural(len(res.Probe.Sigs), "signature")
		}
	}
	return "Scan"
}

// newScanID is a scan's unlisted id: 12 random base-62 characters.
func newScanID() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 12)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

func validScanID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	return true
}

// --- The reader's own scans ---

func (app *Application) sessionScanIDs(ctx context.Context) []string {
	s := app.sessions.GetString(ctx, scanSessionKey)
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func (app *Application) rememberScan(ctx context.Context, id string) {
	ids := append([]string{id}, app.sessionScanIDs(ctx)...)
	if len(ids) > scanOwnMax {
		ids = ids[:scanOwnMax]
	}
	app.sessions.Put(ctx, scanSessionKey, strings.Join(ids, ","))
}

// ownScan is one of the reader's scans, listed.
type ownScan struct {
	ID       string
	Title    string
	Kind     string
	SystemID int64
	System   string
	SecText  string
	SecTier  int
	Ago      string
	At       time.Time
}

func (app *Application) ownScans(ctx context.Context, systemID int64) []ownScan {
	ids := app.sessionScanIDs(ctx)
	if ids == nil {
		ids = []string{}
	}
	rows, err := app.queries.ListOwnScans(ctx, db.ListOwnScansParams{UserID: app.userID(ctx), Ids: ids, MaxRows: scanOwnMax})
	if err != nil {
		logging.Warnf("scans: own scans: %v", err)
		return nil
	}
	names := app.newKillNames(ctx)
	var out []ownScan
	for _, r := range rows {
		if systemID > 0 && r.SystemID != systemID {
			continue
		}
		o := ownScan{ID: r.ID, Kind: r.Kind, SystemID: r.SystemID, At: r.CreatedAt, Ago: ageText(time.Now(), r.CreatedAt)}
		o.Title = scanKindLabels[r.Kind]
		if r.SystemID > 0 {
			sys := names.system(r.SystemID)
			o.System = sys.Name
			o.SecText, o.SecTier = secRound(sys.Security)
		}
		out = append(out, o)
	}
	return out
}

var scanKindLabels = map[string]string{"dscan": "D-scan", "local": "Local", "fleet": "Fleet", "probe": "Probe scan"}

// --- Pages ---

// scanPageView is /intel/scan/ (the paste box) and /intel/scan/{id}/
// (a reading).
type scanPageView struct {
	// The form.
	Raw        string
	SystemName string
	Error      string
	Own        []ownScan

	// A reading.
	ID         string
	Title      string
	Result     scanResult
	At         time.Time
	Ago        string
	SysName    string
	SecText    string
	SecTier    int
	RegionName string
	Mine       bool
	// Killers are a local scan's pilots with recent public kills,
	// most first; OrgNames names every corporation and alliance in
	// it.
	Killers  []scanKiller
	OrgNames map[int64]string
	// TypesGrid and TypesOff are a d-scan's ship types split by
	// the grid line, each counted on its own side of it.
	TypesGrid []scanCount
	TypesOff  []scanCount
	// Groups is a d-scan's full group grid: ship classes beside
	// the structure, deployable, drone and fighter groups and
	// the other objects' kinds, each with its on-grid count.
	Groups []scanCount
	// Sigs are a probe scan's signatures, for display.
	Sigs []probeSig
	// PostTo are the channels the reader may post this scan to.
	PostTo []channelRow
}

// probeSig is one probe result as the page shows it.
type probeSig struct {
	scan.ProbeRow
	Pct  int
	Dist string
}

// sortCounts orders tallies most first, then by name, as tally's
// own listing does.
func sortCounts(list []scanCount) []scanCount {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].N != list[j].N {
			return list[i].N > list[j].N
		}
		return list[i].Name < list[j].Name
	})
	return list
}

// prepScanView fills what the reading's page shows but does not
// store: bar widths, distances.
func prepScanView(v *scanPageView) {
	r := &v.Result
	if d := r.DScan; d != nil {
		d.Classes, d.Types = withPct(d.Classes), withPct(d.Types)
		d.Hulls, d.Roles = withPct(d.Hulls), withPct(d.Roles)
		d.ShipsOff = d.Ships - d.ShipsGrid
		for _, c := range d.Types {
			if c.Grid > 0 {
				v.TypesGrid = append(v.TypesGrid, scanCount{ID: c.ID, Name: c.Name, Class: c.Class, N: c.Grid})
			}
			if off := c.N - c.Grid; off > 0 {
				v.TypesOff = append(v.TypesOff, scanCount{ID: c.ID, Name: c.Name, Class: c.Class, N: off})
			}
		}
		v.TypesGrid = withPct(sortCounts(v.TypesGrid))
		v.TypesOff = withPct(sortCounts(v.TypesOff))
		v.Groups = scanGroupsView(d)
	}
	if f := r.Fleet; f != nil {
		f.Classes, f.Types, f.Systems, f.Wings = withPct(f.Classes), withPct(f.Types), withPct(f.Systems), withPct(f.Wings)
	}
	if p := r.Probe; p != nil {
		p.Groups = withPct(p.Groups)
		for _, s := range p.Sigs {
			ps := probeSig{ProbeRow: s, Pct: int(s.Signal + 0.5)}
			switch {
			case s.DistKm < 0:
				ps.Dist = "–"
			case s.DistKm >= scan.AUKm/10:
				ps.Dist = strconv.FormatFloat(s.DistKm/scan.AUKm, 'f', 2, 64) + " AU"
			default:
				ps.Dist = strconv.FormatFloat(s.DistKm, 'f', 0, 64) + " km"
			}
			v.Sigs = append(v.Sigs, ps)
		}
	}
	if l := r.Local; l != nil {
		top := 1
		for _, g := range l.Alliances {
			top = max(top, g.N)
		}
		for i := range l.Alliances {
			l.Alliances[i].Pct = max(2, l.Alliances[i].N*100/top)
		}
		fillLocalRosters(l)
	}
}

// scanGroupsView folds a d-scan's stored tallies into the one
// group grid the page shows: the ship classes beside the
// structure, deployable, drone and fighter groups (tallied under
// their SDE group names) and the other objects under their kinds,
// each with its total and on-grid count. Celestials stay a bare
// count in the reading's footnote: the stored reading keeps no
// per-group split for them.
func scanGroupsView(d *dscanReading) []scanCount {
	at := map[string]*scanCount{}
	var order []string
	add := func(list []scanCount, key func(scanCount) string) {
		for _, c := range list {
			k := key(c)
			e := at[k]
			if e == nil {
				e = &scanCount{Name: k}
				at[k] = e
				order = append(order, k)
			}
			e.N += c.N
			e.Grid += c.Grid
		}
	}
	byClass := func(fallback string) func(scanCount) string {
		return func(c scanCount) string {
			if c.Class != "" {
				return c.Class
			}
			return fallback
		}
	}
	add(d.Classes, func(c scanCount) string { return c.Name })
	add(d.Structures, byClass("Structure"))
	add(d.Deployables, byClass("Deployable"))
	add(d.DroneTypes, byClass("Drone"))
	add(d.FighterTypes, byClass("Fighter"))
	add(d.Objects, byClass("Other"))
	out := make([]scanCount, 0, len(order))
	for _, k := range order {
		out = append(out, *at[k])
	}
	return withPct(sortCounts(out))
}

// scanRosterCap is how many pilots a local reading prints under
// one corporation; the rest are counted, not listed.
const scanRosterCap = 200

// fillLocalRosters groups a local reading's stored pilot list
// under its corporations for the page, sorted by name and capped
// per corporation. Groups already carrying a roster (a reading
// stored with one) keep it.
func fillLocalRosters(l *localReading) {
	if len(l.Pilots) == 0 {
		return
	}
	byCorp := map[[2]int64][]localPilot{}
	solo := map[int64][]localPilot{}
	for _, p := range l.Pilots {
		if p.AllianceID > 0 {
			k := [2]int64{p.AllianceID, p.CorpID}
			byCorp[k] = append(byCorp[k], p)
		} else {
			solo[p.CorpID] = append(solo[p.CorpID], p)
		}
	}
	capList := func(ps []localPilot) ([]localPilot, int) {
		sort.Slice(ps, func(i, j int) bool { return ps[i].Name < ps[j].Name })
		if len(ps) > scanRosterCap {
			return ps[:scanRosterCap], len(ps) - scanRosterCap
		}
		return ps, 0
	}
	for i := range l.Alliances {
		g := &l.Alliances[i]
		if g.Corp {
			if len(g.Pilots) == 0 {
				g.Pilots, g.MorePilots = capList(solo[g.ID])
			}
			continue
		}
		for j := range g.Corps {
			c := &g.Corps[j]
			if len(c.Pilots) == 0 {
				c.Pilots, c.MorePilots = capList(byCorp[[2]int64{g.ID, c.ID}])
			}
		}
	}
}

// scanKiller is one local pilot with kills on our killboard.
type scanKiller struct {
	localPilot
	Org        string
	Ago        string
	SystemName string
	SecText    string
	SecTier    int
}

func (app *Application) handleScanForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	v := &scanPageView{Own: app.ownScans(ctx, 0)}
	if id, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64); err == nil && id > 0 {
		if sys, err := app.queries.GetSDESystem(ctx, id); err == nil {
			v.SystemName = sys.Name
		}
	}
	data.Scan = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "D-Scan Parser", Description: "Paste a d-scan, local, fleet composition or probe scan for an instant breakdown and a link to share.", CanonicalPath: "/intel/scan/"})
	app.render(ctx, w, http.StatusOK, "scans.html", data)
}

func (app *Application) handleScanSubmit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, scan.MaxBytes+8<<10)
	fail := func(status int, v *scanPageView, msg string) {
		data := app.publicPage(ctx)
		v.Error = msg
		v.Own = app.ownScans(ctx, 0)
		data.Scan = v
		app.render(ctx, w, status, "scans.html", data)
	}
	if err := r.ParseForm(); err != nil {
		fail(http.StatusRequestEntityTooLarge, &scanPageView{}, "That paste is too large: up to 256 KB at a time.")
		return
	}
	v := &scanPageView{Raw: r.PostForm.Get("scan"), SystemName: strings.TrimSpace(r.PostForm.Get("system"))}
	var systemID int64
	if v.SystemName != "" {
		sys, err := app.queries.GetSDESystemByName(ctx, v.SystemName)
		if err != nil {
			fail(http.StatusUnprocessableEntity, v, "There is no system named “"+v.SystemName+"”. Leave it blank to find it from the scan.")
			return
		}
		systemID = sys.SystemID
	}
	p, err := scan.Parse(v.Raw)
	switch {
	case errors.Is(err, scan.ErrEmpty):
		fail(http.StatusUnprocessableEntity, v, "Paste a scan first.")
		return
	case errors.Is(err, scan.ErrTooBig):
		fail(http.StatusRequestEntityTooLarge, v, "That paste is too large: up to 256 KB and 5,000 lines at a time.")
		return
	case err != nil:
		fail(http.StatusUnprocessableEntity, v, "That doesn’t look like a d-scan, local list, fleet composition or probe scan. Copy it from the game with Ctrl+A, Ctrl+C and paste it as is.")
		return
	}
	res := app.readScan(ctx, p, systemID)
	body, err := json.Marshal(res)
	if err != nil {
		fail(http.StatusInternalServerError, v, "That scan could not be saved. Please try again.")
		return
	}
	id, err := newScanID()
	if err == nil {
		err = app.queries.InsertScan(ctx, db.InsertScanParams{ID: id, Kind: string(res.Kind), SystemID: res.SystemID, UserID: app.userID(ctx), Raw: v.Raw, Result: string(body), CreatedAt: time.Now().UTC()})
	}
	if err != nil {
		logging.Errorf("scans: store: %v", err)
		fail(http.StatusInternalServerError, v, "That scan could not be saved. Please try again.")
		return
	}
	app.rememberScan(ctx, id)
	http.Redirect(w, r, "/intel/scan/"+id+"/", http.StatusSeeOther)
}

func (app *Application) handleScanPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	w.Header().Set("X-Robots-Tag", "noindex")
	id := chi.URLParam(r, "id")
	var row db.Scan
	var err error
	if validScanID(id) {
		row, err = app.queries.GetScan(ctx, id)
	} else {
		err = errors.New("bad id")
	}
	v := &scanPageView{ID: id}
	data.Scan = v
	if err != nil {
		v.Error = "This scan does not exist, or it is more than 90 days old."
		app.render(ctx, w, http.StatusNotFound, "scan.html", data)
		return
	}
	if json.Unmarshal([]byte(row.Result), &v.Result) != nil {
		v.Error = "This scan could not be read."
		app.render(ctx, w, http.StatusInternalServerError, "scan.html", data)
		return
	}
	v.Title = scanTitle(v.Result)
	prepScanView(v)
	v.At, v.Ago = row.CreatedAt, ageText(time.Now(), row.CreatedAt)
	v.Raw = row.Raw
	names := app.newKillNames(ctx)
	if v.Result.SystemID > 0 {
		sys := names.system(v.Result.SystemID)
		v.SysName = sys.Name
		v.SecText, v.SecTier = secRound(sys.Security)
		v.RegionName = names.region(sys.RegionID)
	}
	if l := v.Result.Local; l != nil {
		v.OrgNames = map[int64]string{}
		for _, g := range l.Alliances {
			v.OrgNames[g.ID] = g.Name
			for _, c := range g.Corps {
				v.OrgNames[c.ID] = c.Name
			}
		}
		for _, p := range l.Pilots {
			if p.Kills == 0 {
				continue
			}
			k := scanKiller{localPilot: p, Ago: ageText(time.Now(), p.LastKill)}
			if p.AllianceID > 0 {
				k.Org = v.OrgNames[p.AllianceID]
			} else {
				k.Org = v.OrgNames[p.CorpID]
			}
			if p.LastSystem > 0 {
				sys := names.system(p.LastSystem)
				k.SystemName = sys.Name
				k.SecText, k.SecTier = secRound(sys.Security)
			}
			v.Killers = append(v.Killers, k)
		}
		sort.SliceStable(v.Killers, func(i, j int) bool { return v.Killers[i].Kills > v.Killers[j].Kills })
	}
	uid := app.userID(ctx)
	v.Mine = uid > 0 && row.UserID == uid
	if uid > 0 {
		for _, c := range app.accessibleChannels(ctx, app.intelIdentity(ctx)) {
			if c.CanPost {
				v.PostTo = append(v.PostTo, channelRow{ID: c.ID, Name: c.Name})
			}
		}
	}
	for _, own := range app.sessionScanIDs(ctx) {
		if own == id {
			v.Mine = true
		}
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: v.Title, Description: "A scan shared from EveSynapse."})
	app.render(ctx, w, http.StatusOK, "scan.html", data)
}

// handleMapScans is GET /map/scans.json?system=: the reader's own
// scans of one system, for the map's system card.
func (app *Application) handleMapScans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type item struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		At    int64  `json:"at"`
	}
	out := []item{}
	if id, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64); err == nil && id > 0 {
		for _, s := range app.ownScans(ctx, id) {
			out = append(out, item{ID: s.ID, Title: s.Title, At: s.At.Unix()})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// pruneScans drops scans past their 90 days.
func (app *Application) pruneScans(ctx context.Context) {
	n, err := app.queries.PruneScans(ctx, time.Now().Add(-scanRetention))
	if err != nil {
		logging.Warnf("scans: prune: %v", err)
		return
	}
	if n > 0 {
		logging.Infof("scans: pruned %d past 90 days", n)
	}
}
