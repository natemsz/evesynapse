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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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
	Kind     scan.Kind `json:"kind"`
	SystemID int64     `json:"system,omitempty"`
	Skipped  int       `json:"skipped,omitempty"`
	// Residue is the paste lines the parser did not consume,
	// kept with the reading as the parser recorded them.
	Residue []string      `json:"residue,omitempty"`
	DScan   *dscanReading `json:"dscan,omitempty"`
	Local   *localReading `json:"local,omitempty"`
	Fleet   *fleetReading `json:"fleet,omitempty"`
	Probe   *probeReading `json:"probe,omitempty"`
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
	// Prices values the scan's ship hulls per type, priced at
	// paste time through the house pricing order; Value is the
	// priced hulls' total and PricedShips how many of the
	// scan's ships it covers. Hulls only: a d-scan cannot see
	// fittings, so none are ever counted.
	Prices      map[int64]scanTypePrice `json:"prices,omitempty"`
	Value       float64                 `json:"value,omitempty"`
	PricedShips int                     `json:"priced_ships,omitempty"`
}

// scanTypePrice is one ship type's stored hull price: the
// unit figure, the tier of the pricing order that spoke, and
// when that tier's figure was current.
type scanTypePrice struct {
	Unit float64   `json:"unit"`
	Tier string    `json:"tier"`
	AsOf time.Time `json:"as_of,omitempty"`
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
	ID    int64        `json:"id"`
	Name  string       `json:"name"`
	N     int          `json:"n"`
	Kills int64        `json:"kills,omitempty"`
	Corps []localGroup `json:"corps,omitempty"`
	Pct   int          `json:"-"`
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
	res := scanResult{Kind: p.Kind, SystemID: systemID, Skipped: p.Skipped, Residue: p.Residue}
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

// priceScan values a fresh d-scan reading's hulls through
// the house pricing order (AppraiseLines: own swept orders,
// then stored estimates, then the labeled Fuzzwork fallback,
// then an honest gap) and stores the outcome on the reading:
// the per-type prices, their total, and how many of the
// scan's ships they cover. Prices speak from stored rows
// only, as of the paste; a pricing failure leaves the
// reading unpriced rather than failing the paste.
func (app *Application) priceScan(ctx context.Context, d *dscanReading) {
	if d == nil || len(d.Types) == 0 {
		return
	}
	lines := make([]module.AppraisalLine, 0, len(d.Types))
	for _, c := range d.Types {
		if c.ID > 0 && c.N > 0 {
			lines = append(lines, module.AppraisalLine{TypeID: c.ID, Quantity: int64(c.N), Included: true})
		}
	}
	if len(lines) == 0 {
		return
	}
	res, err := app.AppraiseLines(ctx, lines)
	if err != nil {
		logging.Warnf("scans: price scan: %v", err)
		return
	}
	prices, total, covered := scanPricesFromAppraisal(res.Lines)
	if len(prices) == 0 {
		return
	}
	d.Prices, d.Value, d.PricedShips = prices, total, covered
}

// scanPricesFromAppraisal folds appraised lines into a
// reading's stored prices: priced lines only, keyed by type,
// the total their sum, the coverage the ships they price.
// Pure, so the math pins without a database.
func scanPricesFromAppraisal(lines []module.AppraisedLine) (map[int64]scanTypePrice, float64, int) {
	prices := map[int64]scanTypePrice{}
	var total float64
	covered := 0
	for _, ln := range lines {
		if !ln.Priced {
			continue
		}
		prices[ln.TypeID] = scanTypePrice{Unit: ln.UnitPrice, Tier: ln.Tier, AsOf: ln.AsOf}
		total += ln.LineValue
		covered += int(ln.Quantity)
	}
	return prices, total, covered
}

// scanTierLabel words a stored price's tier for the reading
// page, in the appraisal tool's own terms (the scan prices
// on the default basis: The Forge, typical sell).
func scanTierLabel(tier string) string {
	switch tier {
	case module.AppraisalTierTypical:
		return "Forge typical"
	case module.AppraisalTierEstimate:
		return "Estimated sell"
	case module.AppraisalTierFallback:
		return "Fuzzwork aggregate"
	}
	return ""
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

// fusedDistRe finds the figured distances a d-scan row ends
// with, as they survive the client's fusing: kilometres, AU or
// metres. The "-" of a row with no figure is found by the dash
// scan in reconstructFusedDScan -- a name's own dashes must not
// end a row, and only the walk can tell them apart.
var fusedDistRe = regexp.MustCompile(`\d[\d,]*\s*km|\d+(?:[.,]\d+)?\s*AU|\d[\d,]*\s*m\b`)

// splitFirstField splits s at its first run of spaces: the field
// and the rest, both trimmed.
func splitFirstField(s string) (string, string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i+1:])
}

// reconstructFusedDScan re-reads a d-scan whose rows the client
// fused into one line on its way through a chat channel. The
// tabs are gone, but every row still begins with its type ID and
// ends with its distance, and the type's own name -- the SDE's --
// closes the row's middle off: what is left of the middle is the
// row's name. A row counts only when its type resolves in the
// SDE and the middle ends with that type's name; anything else
// in the text is residue. covered and total count letters and
// digits, as the roster bar counts them, so the caller holds
// the reading to the same standard.
func (app *Application) reconstructFusedDScan(ctx context.Context, text string) (scan.Result, int, int) {
	// The row ends: the figured distances, and the dashes that
	// are a row's no-figure marker -- a dash standing as its own
	// field (a space before it) with the next row's type ID or
	// the text's end after it. A name's dash ("System - Name")
	// is followed by a letter and never qualifies.
	type rowEnd struct{ start, end int }
	ends := []rowEnd{}
	for _, loc := range fusedDistRe.FindAllStringIndex(text, -1) {
		ends = append(ends, rowEnd{loc[0], loc[1]})
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '-' || (i > 0 && text[i-1] != ' ' && text[i-1] != '\t') {
			continue
		}
		j := i + 1
		for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
			j++
		}
		if j == len(text) || (text[j] >= '0' && text[j] <= '9') {
			ends = append(ends, rowEnd{i, i + 1})
		}
	}
	if len(ends) == 0 {
		return scan.Result{}, 0, 0
	}
	sort.Slice(ends, func(a, b int) bool { return ends[a].start < ends[b].start })
	locs := make([][2]int, len(ends))
	for i, e := range ends {
		locs[i] = [2]int{e.start, e.end}
	}
	type seg struct {
		head, dist string
		id         int64
		idOK       bool
	}
	segs := make([]seg, 0, len(locs))
	ids, seen := []int64{}, map[int64]bool{}
	prev := 0
	for _, loc := range locs {
		s := seg{head: text[prev:loc[0]], dist: text[loc[0]:loc[1]]}
		if tok, rest := splitFirstField(s.head); rest != "" {
			if id, err := strconv.ParseInt(tok, 10, 64); err == nil && id > 0 {
				s.id, s.idOK = id, true
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		segs = append(segs, s)
		prev = loc[1]
	}
	names := map[int64]string{}
	rows, err := app.queries.ListSDETypeNamesByIDs(ctx, ids)
	if err != nil {
		return scan.Result{}, 0, 0
	}
	for _, r := range rows {
		names[r.TypeID] = r.Name
	}
	out := scan.Result{Kind: scan.KindDScan}
	covered := 0
	for _, s := range segs {
		if !s.idOK {
			continue
		}
		tname, ok := names[s.id]
		if !ok || tname == "" {
			continue
		}
		_, chunk := splitFirstField(s.head)
		var name string
		switch {
		case chunk == tname:
			name = tname
		case strings.HasSuffix(chunk, " "+tname):
			name = strings.TrimSpace(chunk[:len(chunk)-len(tname)])
		default:
			continue
		}
		if name == "" {
			name = tname
		}
		out.DScan = append(out.DScan, scan.DScanRow{TypeID: s.id, Name: name, TypeName: tname, DistKm: scan.ParseDistance(s.dist)})
		covered += countAlnum(s.head) + countAlnum(s.dist)
	}
	return out, covered, countAlnum(text)
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
	return l
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

// ---------------------------------------------------------------------------
// The sentence a reading becomes when it is reported to a channel.
//
// A report's headline is the synthesis a player makes from a paste:
// counts by standing from the receiving channel's perspective, the
// on-grid split, and pilot-to-ship attribution where the paste
// supports it -- a fleet row names its pilot outright, a d-scan
// ship named for a pilot the site already knows is attributed to
// them. Fixed structures (stations, control towers) are
// orientation, not actors: they anchor the sentence ("at the
// Moon 1 station in Teonusude") and are never counted among its
// contents. What happened is read off stored context as well as
// the paste: the channel's known hostiles and its own side, each
// pilot's stored kill activity, and the system's distilled kill
// and NPC figures. The rules are deterministic and run at ingest;
// a report whose sources say nothing about a pilot calls them
// unclassified rather than guessing a standing. A local list alone
// cannot attribute hulls or grid position, and its sentence says
// so.
// ---------------------------------------------------------------------------

// Standings buckets, from the receiving channel's point of view.
const (
	bucketHostile = iota
	bucketFriendly
	bucketNeutral
	bucketUnknown
)

// channelStanding is one channel's standing context: its own
// side and its known hostiles, read once for a report.
type channelStanding struct {
	app   *Application
	ctx   context.Context
	side  intelSide
	watch map[int64]bool
}

func (app *Application) channelStandingOf(ctx context.Context, channelID int64) channelStanding {
	st := channelStanding{app: app, ctx: ctx, side: app.intelSideOf(ctx, channelID), watch: map[int64]bool{}}
	if rows, err := app.queries.ListIntelWatch(ctx, []int64{channelID}); err == nil {
		for _, r := range rows {
			st.watch[r.CharacterID] = true
		}
	}
	return st
}

// of reads one pilot's bucket: the watch list first (a known
// hostile is hostile whatever their affiliation says), then the
// channel's side by affiliation, then a known affiliation off the
// side (neutral), and unknown when no stored source speaks for
// the pilot.
func (st channelStanding) of(id int64) int {
	if st.watch[id] {
		return bucketHostile
	}
	corp, alliance, ok := st.app.pilotAffiliation(st.ctx, id)
	if !ok {
		return bucketUnknown
	}
	if st.side.corps[corp] || (alliance > 0 && st.side.alliances[alliance]) {
		return bucketFriendly
	}
	return bucketNeutral
}

// ofLocal reads a local roster pilot's bucket from the roster's
// own affiliations, which are fresher than the stored ones; a
// roster line with no affiliation falls back to the stored read.
func (st channelStanding) ofLocal(lp localPilot) int {
	if st.watch[lp.ID] {
		return bucketHostile
	}
	if lp.CorpID == 0 && lp.AllianceID == 0 {
		return st.of(lp.ID)
	}
	if st.side.corps[lp.CorpID] || (lp.AllianceID > 0 && st.side.alliances[lp.AllianceID]) {
		return bucketFriendly
	}
	if lp.CorpID > 0 {
		return bucketNeutral
	}
	return bucketUnknown
}

// scanReportText makes the one interpreted sentence a report made
// from a reading carries as its headline, and the standing split
// the sentence was counted from, for the report's stats bar. p is
// the paste the reading was made from, when it can still be had;
// without it the sentence is made from the reading's tallies alone
// and says less. posterID is the reporting pilot, counted friendly
// when a local roster names them.
func (app *Application) scanReportText(ctx context.Context, channelID int64, res scanResult, p *scan.Result, posterID int64) (string, [4]int) {
	st := app.channelStandingOf(ctx, channelID)
	standingOf := st.of
	sysName := ""
	if res.SystemID > 0 {
		sysName = app.newKillNames(ctx).system(res.SystemID).Name
	}
	// The system's stored activity, distilled at ingest from its
	// hourly figures: ship kills and NPC kills in the last day.
	var shipKills24h, npcKills24h int64
	if res.SystemID > 0 {
		if stats, err := app.queries.GetSystemActivityStats(ctx, res.SystemID); err == nil {
			shipKills24h, npcKills24h = stats.ShipKills24h, stats.NpcKills24h
		}
	}
	// pilotOf resolves a name a paste carries to a pilot the site
	// already knows, from the intel name cache. A name never
	// looked up is not resolved here: the report's own analysis
	// queues it, and a later paste attributes it.
	type pilotRef struct {
		id   int64
		name string
		ok   bool
	}
	cache := map[string]pilotRef{}
	pilotOf := func(name string) (int64, string, bool) {
		key := strings.ToLower(name)
		if r, hit := cache[key]; hit {
			return r.id, r.name, r.ok
		}
		r := pilotRef{}
		if row, err := app.queries.GetIntelPilotName(ctx, key); err == nil && row.CharacterID > 0 {
			r = pilotRef{row.CharacterID, row.Name, true}
		}
		cache[key] = r
		return r.id, r.name, r.ok
	}
	// killedHere reads a pilot's stored kill activity: their most
	// recent public kill was in this system, within the week.
	flagMemo := map[int64]bool{}
	killedHere := func(id int64) bool {
		if v, ok := flagMemo[id]; ok {
			return v
		}
		v := false
		if res.SystemID > 0 {
			if rows, err := app.queries.ListPilotKillActivity(ctx, db.ListPilotKillActivityParams{
				CharacterIds: []int64{id}, Since: time.Now().Add(-30 * 24 * time.Hour)}); err == nil {
				for _, r := range rows {
					if r.LastSystem == res.SystemID && time.Since(r.LastKill) <= 7*24*time.Hour {
						v = true
					}
				}
			}
		}
		flagMemo[id] = v
		return v
	}
	switch res.Kind {
	case scan.KindDScan:
		if res.DScan == nil {
			break
		}
		var rows []scan.DScanRow
		var cats map[int64]int64
		if p != nil {
			rows = p.DScan
			if c, err := scanCategories(ctx, app, rows); err == nil {
				cats = c
			} else {
				rows = nil // no categories: fall back to the tallies
			}
		}
		return dscanSentence(res.DScan, rows, cats, sysName, standingOf, pilotOf, killedHere, shipKills24h, npcKills24h)
	case scan.KindLocal:
		if res.Local == nil {
			break
		}
		return localSentence(res.Local, res.SystemID, posterID, time.Now(), st.ofLocal, shipKills24h, npcKills24h)
	case scan.KindFleet:
		if res.Fleet == nil {
			break
		}
		return fleetSentence(res.Fleet, sysName, standingOf, pilotOf, killedHere)
	case scan.KindProbe:
		if res.Probe == nil {
			break
		}
		return probeSentence(res.Probe, sysName), [4]int{}
	}
	return scanTitle(res), [4]int{}
}

// scanCategories reads the SDE category of each distinct type on
// rows, as readDScan reads them.
func scanCategories(ctx context.Context, app *Application, rows []scan.DScanRow) (map[int64]int64, error) {
	ids := make([]int64, 0, len(rows))
	seen := map[int64]bool{}
	for _, r := range rows {
		if !seen[r.TypeID] {
			seen[r.TypeID] = true
			ids = append(ids, r.TypeID)
		}
	}
	cats := map[int64]int64{}
	gs, err := app.queries.ListSDETypeCategories(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range gs {
		cats[c.TypeID] = c.CategoryID
	}
	return cats, nil
}

// hullTally is one hull and how many of it a bucket holds.
type hullTally struct {
	name string
	n    int
}

// tallyHulls sorts a bucket's hulls: most first, then by name.
func tallyHulls(m map[string]int) []hullTally {
	out := make([]hullTally, 0, len(m))
	for name, n := range m {
		out = append(out, hullTally{name, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].name < out[j].name
	})
	return out
}

// bucketText renders one bucket: "3 hostiles (Vexor ×2, Sabre)",
// or a bare count when the bucket holds no named hulls.
func bucketText(singular, pluralForm string, n int, hulls []hullTally) string {
	label := pluralForm
	if n == 1 {
		label = singular
	}
	s := fmt.Sprintf("%d %s", n, label)
	if len(hulls) > 0 {
		parts := make([]string, len(hulls))
		for i, h := range hulls {
			if h.n > 1 {
				parts[i] = fmt.Sprintf("%s ×%d", h.name, h.n)
			} else {
				parts[i] = h.name
			}
		}
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

// standingText renders the counts in the owner's order -- hostiles,
// friendlies, neutrals -- then the unclassified, the ones no
// stored source speaks for.
func standingText(counts [4]int, hulls [4]map[string]int, unknownSingular, unknownPlural string) string {
	parts := []string{
		bucketText("hostile", "hostiles", counts[bucketHostile], tallyHulls(hulls[bucketHostile])),
		bucketText("friendly", "friendly", counts[bucketFriendly], tallyHulls(hulls[bucketFriendly])),
		bucketText("neutral", "neutral", counts[bucketNeutral], tallyHulls(hulls[bucketNeutral])),
	}
	if counts[bucketUnknown] > 0 {
		parts = append(parts, bucketText(unknownSingular, unknownPlural, counts[bucketUnknown], tallyHulls(hulls[bucketUnknown])))
	}
	return strings.Join(parts, ", ")
}

// systemNotes renders what the system's stored activity adds: a
// system with ship kills in the last day is hot; one with heavy
// NPC kills and no ship kills is ratting space, which reads on
// the unclassified and the neutrals standing in it.
func systemNotes(shipKills24h, npcKills24h int64, subjects int) string {
	var notes string
	if shipKills24h >= 5 {
		notes += fmt.Sprintf("; system hot: %d kills in the last 24 hours", shipKills24h)
	}
	if npcKills24h >= 200 && shipKills24h < 5 && subjects > 0 {
		notes += fmt.Sprintf("; %d NPC kills in the last 24 hours — likely ratting traffic", npcKills24h)
	}
	return notes
}

// killedHereNotes renders the flagged pilots: counted neutral or
// unclassified by standing, but their stored kills say they have
// killed in this system within the week.
func killedHereNotes(names []string) string {
	if len(names) == 0 {
		return ""
	}
	if len(names) > 2 {
		return fmt.Sprintf("; %s, %s +%d more killed here this week", names[0], names[1], len(names)-2)
	}
	var out string
	for _, n := range names {
		out += "; " + n + " killed here this week"
	}
	return out
}

var moonRe = regexp.MustCompile(` - Moon (\d+)`)

// dscanAnchor reads where a d-scan sits from its fixed structures:
// a station names its moon in its own name, and a structure on the
// paster's grid (a station at 0 m above all) is where the paster
// was. The anchor is a place, never a count.
func dscanAnchor(rows []scan.DScanRow, cats map[int64]int64, sysName string) string {
	if sysName == "" {
		return ""
	}
	best, bestScore := -1, -1
	for i, r := range rows {
		switch cats[r.TypeID] {
		case catStation, catStructure, catStarbase, catSov:
		default:
			continue
		}
		score := 0
		if cats[r.TypeID] == catStation {
			score += 4
		}
		if r.DistKm == 0 {
			score += 2
		} else if r.DistKm > 0 && r.DistKm <= scanGridKm {
			score++
		}
		if moonRe.MatchString(r.Name) {
			score++
		}
		if score > bestScore {
			bestScore, best = score, i
		}
	}
	if best >= 0 {
		r := rows[best]
		if m := moonRe.FindStringSubmatch(r.Name); m != nil {
			if cats[r.TypeID] == catStation {
				return fmt.Sprintf("At the Moon %s station in %s", m[1], sysName)
			}
			return fmt.Sprintf("At Moon %s in %s", m[1], sysName)
		}
		if cats[r.TypeID] == catStation {
			return "At the station in " + sysName
		}
	}
	return "In " + sysName
}

// shipPair is one attributed ship: the pilot a scan row names, and
// the bucket their standing puts them in.
type shipPair struct {
	pilot  string
	id     int64
	hull   string
	bucket int
}

// pairText renders attribution pairs: "BlackCatRus in the Ibis",
// capped so a fleet-sized list cannot make a headline of itself.
func pairText(pairs []shipPair) string {
	if len(pairs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pairs))
	for i, pr := range pairs {
		if i == 8 {
			parts = append(parts, fmt.Sprintf("and %d more", len(pairs)-8))
			break
		}
		parts = append(parts, pr.pilot+" in the "+pr.hull)
	}
	return " — " + strings.Join(parts, ", ")
}

// dscanSentence synthesises a d-scan: anchored on its structures,
// its ships counted by standing with their hulls, the on-grid
// split over ships alone, and attribution for the ships named for
// a known pilot. Rows nil (the paste is gone) reads the tallies
// instead: every ship unclassified, no attribution.
func dscanSentence(d *dscanReading, rows []scan.DScanRow, cats map[int64]int64, sysName string,
	standingOf func(int64) int, pilotOf func(string) (int64, string, bool), killedHere func(int64) bool,
	shipKills24h, npcKills24h int64) (string, [4]int) {

	var counts [4]int
	hulls := [4]map[string]int{}
	for b := range hulls {
		hulls[b] = map[string]int{}
	}
	grid := 0
	var pairs []shipPair
	if rows != nil {
		for _, r := range rows {
			if cats[r.TypeID] != catShip {
				continue
			}
			bucket := bucketUnknown
			if r.Name != "" && !strings.EqualFold(r.Name, r.TypeName) {
				if id, name, ok := pilotOf(r.Name); ok {
					bucket = standingOf(id)
					pairs = append(pairs, shipPair{pilot: name, id: id, hull: r.TypeName, bucket: bucket})
				}
			}
			counts[bucket]++
			hulls[bucket][r.TypeName]++
			if r.DistKm >= 0 && r.DistKm <= scanGridKm {
				grid++
			}
		}
	} else {
		for _, t := range d.Types {
			counts[bucketUnknown] += t.N
			hulls[bucketUnknown][t.Name] += t.N
		}
		grid = d.ShipsGrid
	}
	total := counts[bucketHostile] + counts[bucketFriendly] + counts[bucketNeutral] + counts[bucketUnknown]
	var flagged []string
	seenFlag := map[int64]bool{}
	for _, pr := range pairs {
		if (pr.bucket == bucketNeutral || pr.bucket == bucketUnknown) && !seenFlag[pr.id] && killedHere(pr.id) {
			seenFlag[pr.id] = true
			flagged = append(flagged, pr.pilot)
		}
	}
	var b strings.Builder
	if anchor := dscanAnchor(rows, cats, sysName); anchor != "" {
		b.WriteString(anchor + ": ")
	} else if sysName != "" {
		b.WriteString("In " + sysName + ": ")
	}
	if total == 0 {
		b.WriteString("no ships on scan")
	} else {
		b.WriteString(standingText(counts, hulls, "unclassified ship", "unclassified ships"))
		b.WriteString(fmt.Sprintf(", %d on grid", grid))
		b.WriteString(pairText(pairs))
	}
	b.WriteString(killedHereNotes(flagged))
	b.WriteString(systemNotes(shipKills24h, npcKills24h, counts[bucketNeutral]+counts[bucketUnknown]))
	b.WriteString(".")
	return b.String(), counts
}

// hostileClusters reads the hostile names for the groups a player
// spots: a surname three or more of them carry, and a surname
// whose first names share a four-letter stem -- one player's
// alts, more likely than a family. It returns descriptions of
// the groups (largest first, at most three) and how many names
// the alt groups over-count the pilots behind them by.
func hostileClusters(names []string) (descs []string, altCut int) {
	type group struct {
		names   []string
		surname string
		first   int
	}
	bySurname := map[string]*group{}
	byAlt := map[string]*group{}
	var surnames, alts []*group
	add := func(m map[string]*group, order *[]*group, key, name, surname string, idx int) {
		g := m[key]
		if g == nil {
			g = &group{surname: surname, first: idx}
			m[key] = g
			*order = append(*order, g)
		}
		g.names = append(g.names, name)
	}
	for i, name := range names {
		fields := strings.Fields(name)
		if len(fields) < 2 {
			continue
		}
		first, last := fields[0], fields[len(fields)-1]
		add(bySurname, &surnames, strings.ToLower(last), name, last, i)
		if len(first) >= 4 {
			add(byAlt, &alts, strings.ToLower(last)+"|"+strings.ToLower(first[:4]), name, last, i)
		}
	}
	covered := map[*group]bool{} // surname groups an alt group already tells
	type desc struct {
		text string
		n    int
		at   int
	}
	var found []desc
	for _, g := range alts {
		if len(g.names) < 2 {
			continue
		}
		altCut += len(g.names) - 1
		firsts := make([]string, len(g.names))
		for i, n := range g.names {
			firsts[i] = strings.Fields(n)[0]
		}
		found = append(found, desc{strings.Join(firsts, "/") + " " + g.surname, len(g.names), g.first})
		if s := bySurname[strings.ToLower(g.surname)]; s != nil && len(s.names) == len(g.names) {
			covered[s] = true
		}
	}
	for _, g := range surnames {
		if len(g.names) >= 3 && !covered[g] {
			found = append(found, desc{fmt.Sprintf("%s ×%d", g.surname, len(g.names)), len(g.names), g.first})
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].n != found[j].n {
			return found[i].n > found[j].n
		}
		return found[i].at < found[j].at
	})
	for i, d := range found {
		if i == 3 {
			break
		}
		descs = append(descs, d.text)
	}
	return descs, altCut
}

// localSentence synthesises a local roster: its size, its counts
// by standing with the reporting pilot counted friendly when the
// roster names them, the hostile name clusters, and the honest
// gap -- a local list alone says nothing about ships or grid, and
// the sentence says so. Neutral and unclassified pilots whose
// stored kills put them in this system within the week are
// flagged: standing alone understates them.
func localSentence(l *localReading, systemID, posterID int64, now time.Time,
	standingOf func(localPilot) int, shipKills24h, npcKills24h int64) (string, [4]int) {

	var counts [4]int
	inclYou := false
	var hostileNames, flagged []string
	for _, p := range l.Pilots {
		bucket := standingOf(p)
		if posterID > 0 && p.ID == posterID {
			bucket = bucketFriendly
			inclYou = true
		}
		counts[bucket]++
		if bucket == bucketHostile {
			hostileNames = append(hostileNames, p.Name)
		}
		if (bucket == bucketNeutral || bucket == bucketUnknown) && systemID > 0 &&
			p.LastSystem == systemID && !p.LastKill.IsZero() && now.Sub(p.LastKill) <= 7*24*time.Hour {
			flagged = append(flagged, p.Name)
		}
	}
	total := len(l.Pilots) + len(l.Unknown)
	head := fmt.Sprintf("%d in local", total)
	if l.Partial {
		head = fmt.Sprintf("%d in local (partial roster)", total)
	}
	parts := []string{
		bucketText("hostile", "hostiles", counts[bucketHostile], nil),
		bucketText("friendly", "friendly", counts[bucketFriendly], nil),
		bucketText("neutral", "neutral", counts[bucketNeutral], nil),
	}
	if inclYou {
		parts[1] += " (incl. you)"
	}
	if counts[bucketUnknown] > 0 {
		parts = append(parts, fmt.Sprintf("%d unclassified", counts[bucketUnknown]))
	}
	if len(l.Unknown) > 0 {
		parts = append(parts, fmt.Sprintf("%d unresolved", len(l.Unknown)))
	}
	s := head + ": " + strings.Join(parts, ", ")
	if descs, altCut := hostileClusters(hostileNames); len(descs) > 0 {
		s += "; clusters: " + strings.Join(descs, ", ")
		if altCut > 0 {
			s += fmt.Sprintf(" (%d hostile names ≈ %d pilots)", counts[bucketHostile], counts[bucketHostile]-altCut)
		}
	}
	s += " — no scan attached, ships unknown"
	s += killedHereNotes(flagged)
	s += systemNotes(shipKills24h, npcKills24h, counts[bucketNeutral]+counts[bucketUnknown]+len(l.Unknown))
	// The split a stats bar shows folds the roster's unresolved
	// names into the unclassified: the sentence keeps them apart,
	// a proportion does not need to.
	counts[bucketUnknown] += len(l.Unknown)
	return s + ".", counts
}

// fleetSentence synthesises a fleet composition: its members are
// named by their rows outright, so attribution is plain, and the
// counts by standing carry each bucket's hulls. A composition
// says nothing about grid, and the sentence claims none.
func fleetSentence(f *fleetReading, sysName string,
	standingOf func(int64) int, pilotOf func(string) (int64, string, bool), killedHere func(int64) bool) (string, [4]int) {

	var counts [4]int
	hulls := [4]map[string]int{}
	for b := range hulls {
		hulls[b] = map[string]int{}
	}
	var pairs []shipPair
	for _, m := range f.Members {
		bucket := bucketUnknown
		id, _, known := pilotOf(m.Name)
		if known {
			bucket = standingOf(id)
		}
		counts[bucket]++
		if m.Ship != "" {
			hulls[bucket][m.Ship]++
			pairs = append(pairs, shipPair{pilot: m.Name, id: id, hull: m.Ship, bucket: bucket})
		}
	}
	var flagged []string
	seenFlag := map[int64]bool{}
	for _, pr := range pairs {
		if pr.id > 0 && (pr.bucket == bucketNeutral || pr.bucket == bucketUnknown) && !seenFlag[pr.id] && killedHere(pr.id) {
			seenFlag[pr.id] = true
			flagged = append(flagged, pr.pilot)
		}
	}
	var b strings.Builder
	if sysName != "" {
		b.WriteString("In " + sysName + ": ")
	}
	b.WriteString(fmt.Sprintf("fleet of %d — ", len(f.Members)))
	b.WriteString(standingText(counts, hulls, "unclassified pilot", "unclassified pilots"))
	b.WriteString(pairText(pairs))
	b.WriteString(killedHereNotes(flagged))
	b.WriteString(".")
	return b.String(), counts
}

// probeSentence synthesises probe results: signatures are the
// content; nothing in them is a pilot or a ship.
func probeSentence(pr *probeReading, sysName string) string {
	s := ""
	if sysName != "" {
		s = "In " + sysName + ": "
	}
	s += plural(len(pr.Sigs), "signature")
	if pr.Unresolved > 0 {
		s += fmt.Sprintf(", %d unresolved", pr.Unresolved)
	}
	return s + "."
}

// scanHostiles is a reading's headline count, the number a
// report made from it carries as its hostiles.
func scanHostiles(res scanResult) int32 {
	switch {
	case res.DScan != nil:
		return int32(res.DScan.Ships)
	case res.Local != nil:
		return int32(len(res.Local.Pilots) + len(res.Local.Unknown))
	case res.Fleet != nil:
		return int32(len(res.Fleet.Members))
	}
	return 0
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

// --- Groups and related scans ---

const (
	// scanGroupMax caps a group timeline on a reading's page.
	scanGroupMax = 25
	// scanRelatedMax caps the related-scans rail.
	scanRelatedMax = 8
	// scanRelatedWindow is how far back a same-system scan
	// counts as related.
	scanRelatedWindow = 7 * 24 * time.Hour
)

// scanGroupAppend decides whether a paste may join the group
// named by a group id -- the founding scan's own id. A group
// founded signed in takes appends only from its founder's
// account; an anonymous group takes them from anyone holding
// the link.
func (app *Application) scanGroupAppend(ctx context.Context, groupID string) (string, bool) {
	if !validScanID(groupID) {
		return "", false
	}
	founder, err := app.queries.GetScan(ctx, groupID)
	if err != nil {
		return "", false
	}
	if founder.UserID > 0 && app.userID(ctx) != founder.UserID {
		return "", false
	}
	return groupID, true
}

// scanRail builds a reading page's group timeline and
// related-scans rail, and picks the scan its Compare button
// diffs against: the previous scan of the same kind in the
// group, else the nearest related one. Related means the
// same system within the week, outside the group; every
// delta is computed from the stored readings.
func (app *Application) scanRail(ctx context.Context, row db.GetScanRow, cur scanResult) (*scanGroupView, []scanRailEntry, string) {
	now := time.Now()
	groupID := row.GroupID
	if groupID == "" {
		groupID = row.ID
	}
	var group *scanGroupView
	compareWith := ""
	if rows, err := app.queries.ListScanGroup(ctx, db.ListScanGroupParams{GroupID: groupID, MaxRows: scanGroupMax}); err == nil {
		g := &scanGroupView{ID: groupID}
		if _, ok := app.scanGroupAppend(ctx, groupID); ok {
			g.CanAppend = true
		}
		type prevScan struct {
			res scanResult
			at  time.Time
			id  string
		}
		prevByKind := map[scan.Kind]prevScan{}
		var pairAt time.Duration = -1
		for _, gr := range rows {
			var res scanResult
			if json.Unmarshal([]byte(gr.Result), &res) != nil {
				continue
			}
			e := scanRailEntry{ID: gr.ID, Kind: scanKindLabels[gr.Kind], Title: scanTitle(res),
				At: gr.CreatedAt, Ago: ageText(now, gr.CreatedAt), Current: gr.ID == row.ID, SameKind: res.Kind == cur.Kind}
			if prev, ok := prevByKind[res.Kind]; ok {
				e.Delta, e.Notes = scanDeltaBetween(prev.res, prev.at, res, gr.CreatedAt)
				if e.Current {
					compareWith = prev.id
				}
			}
			if e.Current {
				// The paired scan of another kind nearest
				// this one in time, either way.
				for _, orow := range rows {
					if orow.ID == row.ID || orow.Kind == string(cur.Kind) {
						continue
					}
					d := orow.CreatedAt.Sub(row.CreatedAt)
					if d < 0 {
						d = -d
					}
					if pairAt < 0 || d < pairAt {
						pairAt = d
						g.Pair = scanPairText(orow, row)
					}
				}
			}
			prevByKind[res.Kind] = prevScan{res, gr.CreatedAt, gr.ID}
			g.Entries = append(g.Entries, e)
		}
		if len(g.Entries) > 1 || g.CanAppend {
			group = g
		}
	}
	var related []scanRailEntry
	if row.SystemID > 0 {
		if rows, err := app.queries.ListRelatedScans(ctx, db.ListRelatedScansParams{
			SystemID: row.SystemID, ExcludeID: row.ID, ExcludeGroup: row.GroupID,
			Since: now.Add(-scanRelatedWindow), MaxRows: scanRelatedMax,
		}); err == nil {
			for _, rr := range rows {
				var res scanResult
				if json.Unmarshal([]byte(rr.Result), &res) != nil {
					continue
				}
				e := scanRailEntry{ID: rr.ID, Kind: scanKindLabels[rr.Kind], Title: scanTitle(res),
					At: rr.CreatedAt, Ago: ageText(now, rr.CreatedAt), SameKind: res.Kind == cur.Kind}
				if e.SameKind {
					e.Delta, e.Notes = scanDeltaBetween(cur, row.CreatedAt, res, rr.CreatedAt)
					if compareWith == "" {
						compareWith = e.ID
					}
				}
				related = append(related, e)
			}
		} else {
			logging.Warnf("scans: related: %v", err)
		}
	}
	return group, related, compareWith
}

// scanPairText names a group sibling of the other kind
// against this scan: "Local from 12 min before this scan".
func scanPairText(other db.ListScanGroupRow, row db.GetScanRow) string {
	d := other.CreatedAt.Sub(row.CreatedAt)
	when := "before"
	if d > 0 {
		when = "after"
	} else {
		d = -d
	}
	return scanKindLabels[other.Kind] + " from " + scanRelDur(d) + " " + when + " this scan"
}

// scanRelDur is a short duration for scan-to-scan text:
// "45 sec", "12 min", "3 hr", "2 days".
func scanRelDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + " sec"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " hr"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " days"
	}
}

// scanDeltaBetween words the difference between two
// readings of the same kind: the headline delta of the
// second against the first, and the notable moves between
// them in time order (the earlier reading against the
// later, whichever side of the page each sits on).
func scanDeltaBetween(a scanResult, aAt time.Time, b scanResult, bAt time.Time) (string, []string) {
	if a.Kind != b.Kind {
		return "", nil
	}
	earlier, later := a, b
	if bAt.Before(aAt) {
		earlier, later = b, a
	}
	switch a.Kind {
	case scan.KindDScan:
		if a.DScan == nil || b.DScan == nil {
			return "", nil
		}
		return signedScanDelta(b.DScan.Ships-a.DScan.Ships, "ship"), notableMoves(earlier.DScan, later.DScan)
	case scan.KindLocal:
		if a.Local == nil || b.Local == nil {
			return "", nil
		}
		arrived, left := localMoves(earlier.Local, later.Local)
		return signedScanDelta(len(b.Local.Pilots)-len(a.Local.Pilots), "pilot"), countMoves(arrived, left)
	case scan.KindFleet:
		if a.Fleet == nil || b.Fleet == nil {
			return "", nil
		}
		return signedScanDelta(len(b.Fleet.Members)-len(a.Fleet.Members), "member"), nil
	case scan.KindProbe:
		if a.Probe == nil || b.Probe == nil {
			return "", nil
		}
		return signedScanDelta(len(b.Probe.Sigs)-len(a.Probe.Sigs), "signature"), nil
	}
	return "", nil
}

// signedScanDelta words a count difference: "+7 ships",
// "-2 ships", "" when even.
func signedScanDelta(d int, noun string) string {
	if d == 0 {
		return ""
	}
	sign := "+"
	if d < 0 {
		sign = "-"
		d = -d
	}
	return sign + plural(d, noun)
}

// notableMoves lists the notable-hull moves between two
// d-scan readings, earlier first: hulls the later reading
// has that the earlier did not ("Avatar arrived") and the
// reverse ("Nyx left"). Capped at three.
func notableMoves(a, b *dscanReading) []string {
	inA := map[string]bool{}
	for _, c := range a.Notable {
		inA[c.Name] = true
	}
	inB := map[string]bool{}
	for _, c := range b.Notable {
		inB[c.Name] = true
	}
	var out []string
	for _, c := range b.Notable {
		if !inA[c.Name] {
			out = append(out, c.Name+" arrived")
		}
	}
	for _, c := range a.Notable {
		if !inB[c.Name] {
			out = append(out, c.Name+" left")
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// localMoves counts pilots arriving and leaving between
// two local readings, earlier first.
func localMoves(a, b *localReading) (arrived, left int) {
	inA := map[int64]bool{}
	for _, p := range a.Pilots {
		inA[p.ID] = true
	}
	inB := map[int64]bool{}
	for _, p := range b.Pilots {
		inB[p.ID] = true
	}
	for id := range inB {
		if !inA[id] {
			arrived++
		}
	}
	for id := range inA {
		if !inB[id] {
			left++
		}
	}
	return arrived, left
}

// countMoves words pilot moves between two local readings.
func countMoves(arrived, left int) []string {
	var out []string
	if arrived > 0 {
		out = append(out, plural(arrived, "arrived"))
	}
	if left > 0 {
		out = append(out, plural(left, "left"))
	}
	return out
}

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
	// Sigs are a probe scan's signatures, for display.
	Sigs []probeSig
	// PostTo are the channels the reader may post this scan to.
	PostTo []channelRow
	// Group is the scan's group (schema 086) as this page
	// shows it, Related the same system's recent scans
	// outside the group; both carry deltas against this
	// reading or the group's previous scan of a kind.
	Group   *scanGroupView
	Related []scanRailEntry
	// CompareWith is a nearby scan of the same kind this one
	// can be diffed against, "" when there is none.
	CompareWith string
	// CanCorrect: the reader pasted this scan (their account
	// or their session) and may fix its system.
	CanCorrect bool
	// HasRail: the reading renders with the group, related
	// and tools rail beside it — set when any of them shows.
	HasRail bool
	// TypeValues is a d-scan's priced hulls, biggest first;
	// ValueText the total, short.
	TypeValues []scanTypeValue
	ValueText  string
	// StripComp and StripGrid are the glanceable strips
	// under a d-scan's summary: the hull composition and
	// the on/off-grid split as proportional segments of the
	// stored totals, counts labeled in their legends.
	StripComp []scanStripSeg
	StripGrid []scanStripSeg
}

// scanStripSeg is one segment of a proportional strip: its
// label, its count, its share of the whole in percent
// (the legend's figure; the segment's width is its count
// against the others, set by flex), and its tone — an
// index into the strip palette classes in scans.css (the
// CSP keeps style attributes out of the markup, so colors
// and widths arrive as classes and custom properties).
type scanStripSeg struct {
	Name string
	N    int
	Pct  int
	Tone int
}

// scanRailEntry is one scan in a group timeline or the
// related-scans rail: its headline, and how it differs from
// the scan it is listed beside.
type scanRailEntry struct {
	ID       string
	Kind     string // "D-scan", "Local", ...
	Title    string
	At       time.Time
	Ago      string
	Current  bool
	SameKind bool
	Delta    string   // "+7 ships" against the reference scan
	Notes    []string // "Avatar arrived", "3 left"
}

// scanGroupView is a scan group as one member's page shows
// it: the timeline, oldest first, the paired scan of the
// other kind when one sits near this one in time, and
// whether the reader may append to the group.
type scanGroupView struct {
	ID        string // the founding scan's id
	Entries   []scanRailEntry
	Pair      string
	CanAppend bool
}

// scanTypeValue is one priced hull type on the reading page.
type scanTypeValue struct {
	ID        int64
	Name      string
	Class     string
	N         int
	UnitText  string
	LineText  string
	TierLabel string
	AsOf      string // the tier figure's date, estimates and the fallback only
	line      float64
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
		if d.Ships > 0 {
			// The glanceable strips: composition from the
			// stored hull tallies (classes for a reading
			// stored before hulls), the grid split from the
			// stored counts. Top five segments stand; the
			// rest fold into Other.
			src := d.Hulls
			if len(src) == 0 {
				src = d.Classes
			}
			for i, c := range src {
				if i == 5 && len(src) > 6 {
					other := 0
					for _, rest := range src[5:] {
						other += rest.N
					}
					v.StripComp = append(v.StripComp, scanStripSeg{Name: "Other", N: other,
						Pct: other * 100 / d.Ships, Tone: 7})
					break
				}
				if i >= 6 {
					break
				}
				v.StripComp = append(v.StripComp, scanStripSeg{Name: c.Name, N: c.N,
					Pct: c.N * 100 / d.Ships, Tone: i})
			}
			v.StripGrid = []scanStripSeg{
				{Name: "On grid", N: d.ShipsGrid, Pct: d.ShipsGrid * 100 / d.Ships, Tone: 0},
				{Name: "Off grid", N: d.ShipsOff, Pct: d.ShipsOff * 100 / d.Ships, Tone: 6},
			}
		}
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
		if d.Value > 0 && len(d.Prices) > 0 {
			v.ValueText = formatISKShort(d.Value)
			vals := make([]scanTypeValue, 0, len(d.Prices))
			for _, c := range d.Types {
				p, ok := d.Prices[c.ID]
				if !ok {
					continue
				}
				tv := scanTypeValue{ID: c.ID, Name: c.Name, Class: c.Class, N: c.N,
					UnitText: esi.FormatISK(p.Unit), LineText: esi.FormatISK(p.Unit * float64(c.N)),
					TierLabel: scanTierLabel(p.Tier), line: p.Unit * float64(c.N)}
				if (p.Tier == module.AppraisalTierEstimate || p.Tier == module.AppraisalTierFallback) && !p.AsOf.IsZero() {
					tv.AsOf = p.AsOf.Format("2006-01-02")
				}
				vals = append(vals, tv)
			}
			sort.SliceStable(vals, func(i, j int) bool { return vals[i].line > vals[j].line })
			if len(vals) > 12 {
				vals = vals[:12]
			}
			v.TypeValues = vals
		}
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
	// A group field files the paste under an existing scan
	// group (schema 086) instead of starting a new one.
	groupID := ""
	if g := strings.TrimSpace(r.PostForm.Get("group")); g != "" {
		var ok bool
		groupID, ok = app.scanGroupAppend(ctx, g)
		if !ok {
			fail(http.StatusUnprocessableEntity, v, "That scan group does not take new scans from here.")
			return
		}
	}
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
	app.priceScan(ctx, res.DScan)
	body, err := json.Marshal(res)
	if err != nil {
		fail(http.StatusInternalServerError, v, "That scan could not be saved. Please try again.")
		return
	}
	id, err := newScanID()
	if err == nil {
		if groupID == "" {
			// A fresh paste starts a group named for itself.
			groupID = id
		}
		err = app.queries.InsertScan(ctx, db.InsertScanParams{ID: id, Kind: string(res.Kind), SystemID: res.SystemID, UserID: app.userID(ctx), GroupID: groupID, Raw: v.Raw, Result: string(body), CreatedAt: time.Now().UTC()})
	}
	if err != nil {
		logging.Errorf("scans: store: %v", err)
		fail(http.StatusInternalServerError, v, "That scan could not be saved. Please try again.")
		return
	}
	// The pilots this local named are met: each is offered a
	// history backfill (pilot_activity.go). Channel postings of a
	// scan meet theirs through the report's analysis instead.
	if res.Local != nil {
		ids := make([]int64, 0, len(res.Local.Pilots))
		for _, lp := range res.Local.Pilots {
			ids = append(ids, lp.ID)
		}
		app.notePilotsEncountered(ids)
	}
	app.rememberScan(ctx, id)
	http.Redirect(w, r, "/intel/scan/"+id+"/", http.StatusSeeOther)
}

func (app *Application) handleScanPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	w.Header().Set("X-Robots-Tag", "noindex")
	id := chi.URLParam(r, "id")
	var row db.GetScanRow
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
	if uid > 0 && row.SystemID > 0 {
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
	v.CanCorrect = v.Mine
	v.Group, v.Related, v.CompareWith = app.scanRail(ctx, row, v.Result)
	v.HasRail = v.Group != nil || len(v.Related) > 0 || v.CanCorrect
	metaTitle := v.Title
	if v.SysName != "" {
		metaTitle += " · " + v.SysName
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: metaTitle, Description: scanDescription(v),
		CanonicalPath: "/intel/scan/" + id + "/", ImagePath: "/intel/scan/" + id + "/og.png"})
	app.render(ctx, w, http.StatusOK, "scan.html", data)
}

// scanDescription is a reading's one-line summary for link
// unfurls: kind, place, and the headline counts.
func scanDescription(v *scanPageView) string {
	r := v.Result
	place := "an unknown system"
	if v.SysName != "" {
		place = v.SysName
	}
	switch {
	case r.DScan != nil:
		s := "D-scan in " + place + ": " + plural(r.DScan.Ships, "ship") + ", " +
			strconv.Itoa(r.DScan.ShipsGrid) + " on grid."
		if r.DScan.Value > 0 {
			s += " Hull value ≈ " + formatISKShort(r.DScan.Value) + " ISK."
		}
		return s
	case r.Local != nil:
		return "Local scan in " + place + ": " + plural(len(r.Local.Pilots)+len(r.Local.Unknown), "pilot") +
			" in " + plural(len(r.Local.Alliances), "group") + "."
	case r.Fleet != nil:
		return "Fleet composition: " + plural(len(r.Fleet.Members), "member") + "."
	case r.Probe != nil:
		return "Probe scan in " + place + ": " + plural(len(r.Probe.Sigs), "signature") + "."
	}
	return "A scan shared from EveSynapse."
}

// handleScanSystem is POST /intel/scan/{id}/system: whoever
// pasted the scan corrects its system. The scans row and the
// stored reading move together, so the page, the map's scan
// lists, the group and related rails and channel posting all
// read the corrected system; reports already posted from
// the scan keep the system they were filed under.
func (app *Application) handleScanSystem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	back := app.flashBack(w, r, "/intel/scan/"+id+"/")
	if !validScanID(id) {
		back("This scan does not exist.")
		return
	}
	row, err := app.queries.GetScan(ctx, id)
	if err != nil {
		back("This scan does not exist.")
		return
	}
	uid := app.userID(ctx)
	mine := uid > 0 && row.UserID == uid
	for _, own := range app.sessionScanIDs(ctx) {
		if own == id {
			mine = true
		}
	}
	if !mine {
		back("Only whoever pasted this scan can correct its system.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("system"))
	sys, err := app.queries.GetSDESystemByName(ctx, name)
	if err != nil {
		back("There is no system named “" + name + "”.")
		return
	}
	var res scanResult
	if json.Unmarshal([]byte(row.Result), &res) != nil {
		back("This scan could not be read.")
		return
	}
	res.SystemID = sys.SystemID
	body, err := json.Marshal(res)
	if err != nil {
		back("That did not go through — please try again.")
		return
	}
	if err := app.queries.UpdateScanSystem(ctx, db.UpdateScanSystemParams{ID: id, SystemID: sys.SystemID, Result: string(body)}); err != nil {
		logging.Errorf("scans: correct system: %v", err)
		back("That did not go through — please try again.")
		return
	}
	back("System set to " + sys.Name + ".")
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
