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
	catDrone      = 18
	catDeployable = 22
	catStarbase   = 23
	catStructure  = 65
	catFighter    = 87
)

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
	Items       int         `json:"items"`
	Ships       int         `json:"ships"`
	ShipsGrid   int         `json:"ships_grid"`
	Classes     []scanCount `json:"classes"`
	Types       []scanCount `json:"types"`
	Structures  []scanCount `json:"structures,omitempty"`
	Deployables []scanCount `json:"deployables,omitempty"`
	Drones      int         `json:"drones,omitempty"`
	Fighters    int         `json:"fighters,omitempty"`
	Celestials  int         `json:"celestials,omitempty"`
	Other       int         `json:"other,omitempty"`
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
	sysVotes := map[string]int{}
	for _, r := range rows {
		grid := r.DistKm >= 0 && r.DistKm <= scanGridKm
		switch cat[r.TypeID] {
		case catShip:
			d.Ships++
			if grid {
				d.ShipsGrid++
			}
			cl := class[r.TypeID]
			if cl == "" {
				cl = "Ship"
			}
			classes.add(0, cl, "", grid)
			types.add(r.TypeID, r.TypeName, cl, grid)
		case catStructure, catStarbase, catStation:
			structs.add(r.TypeID, r.TypeName, class[r.TypeID], grid)
			// Stations carry their moon's name; player structures
			// are named "System - Name".
			if s := scan.SystemFromCelestial(r.Name); s != "" {
				sysVotes[s]++
			} else if i := strings.Index(r.Name, " - "); i > 0 {
				sysVotes[r.Name[:i]]++
			}
		case catDeployable:
			deploy.add(r.TypeID, r.TypeName, class[r.TypeID], grid)
		case catDrone:
			d.Drones++
		case catFighter:
			d.Fighters++
		case catCelestial:
			d.Celestials++
			if s := scan.SystemFromCelestial(r.Name); s != "" {
				sysVotes[s]++
			}
		default:
			d.Other++
		}
	}
	d.Classes, d.Types, d.Structures, d.Deployables = classes.sorted(), types.sorted(), structs.sorted(), deploy.sorted()
	// The system: the name most celestials carry, if the SDE knows
	// it (a few lookups at most).
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
		if i == 3 {
			break
		}
		if sys, err := app.queries.GetSDESystemByName(ctx, s); err == nil {
			return d, sys.SystemID
		}
	}
	return d, 0
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

// prepScanView fills what the reading's page shows but does not
// store: bar widths, distances.
func prepScanView(v *scanPageView) {
	r := &v.Result
	if d := r.DScan; d != nil {
		d.Classes, d.Types = withPct(d.Classes), withPct(d.Types)
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
