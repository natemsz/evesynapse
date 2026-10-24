package app

// ---------------------------------------------------------------------------
// The intel workbench (/intel/workbench/{reportID}/): one report
// opened up -- its stored scan or roster reading broken down the
// way the scan tool breaks a paste down, its pilots with what the
// site has stored about them, the difference against the previous
// report in the same channel and system, and the system's gates
// with their stored activity. Everything renders from stored
// rows: the report, its scan reading and the paste it was made
// from, settled pilot records, place activity distilled at
// ingest. Nothing is fetched and nothing is invented: a field no
// stored source speaks for is left out, never filled in.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/logging"
	"evesynapse/internal/scan"
)

// wbDossierCap bounds how many roster pilots get their settled
// pilot record read for the page: more than any roster a report
// carries in practice, and a hard stop so a huge local paste
// cannot turn one render into an unbounded read.
const wbDossierCap = 200

// --- Roles ---------------------------------------------------------------

// Workbench roles: the summary box's buckets, by SDE group. The
// labels are the design's; the membership is the SDE's group for
// the type, nothing inferred per hull.
var (
	wbRoleOrder = []string{"dictor", "recon", "logi", "command", "capital", "subcap", "probes"}
	wbRoleLabel = map[string]string{
		"dictor":  "Interdictors / HICs",
		"recon":   "Force Recon / Cyno",
		"logi":    "Logistics",
		"command": "Command / Links",
		"capital": "Capitals / Supers",
		"subcap":  "Subcap DPS / Tackle",
		"probes":  "Probes & Bubbles",
	}
	// The role chip a single hull type carries in the table.
	wbRoleChip = map[string]string{
		"dictor":  "Interdictor / Bubble",
		"recon":   "Force Recon / Cyno",
		"logi":    "Logistics",
		"command": "Command / Links",
		"capital": "Capital / Super",
		"subcap":  "Subcap DPS / Tackle",
		"probes":  "Probes / Bubbles",
	}
	wbCapitalGroups = map[string]bool{
		"Titan": true, "Supercarrier": true, "Carrier": true, "Dreadnought": true,
		"Lancer Dreadnought": true, "Force Auxiliary": true,
		"Capital Industrial Ship": true, "Command Carrier": true,
	}
)

// wbRoleOf is a ship type's workbench role from its SDE group.
func wbRoleOf(group string) string {
	switch {
	case group == "Interdictor" || group == "Heavy Interdiction Cruiser":
		return "dictor"
	case group == "Force Recon Ship":
		return "recon"
	case group == "Logistics" || group == "Logistics Frigate":
		return "logi"
	case group == "Command Ship" || group == "Command Destroyer":
		return "command"
	case wbCapitalGroups[group]:
		return "capital"
	default:
		return "subcap"
	}
}

// wbCaps are the capability lines a hull class carries in the
// drawer: what the class is for, in the design's own words, from
// the SDE group alone.
func wbCaps(group string) []string {
	switch group {
	case "Interdictor":
		return []string{"Interdiction Sphere Launcher"}
	case "Heavy Interdiction Cruiser":
		return []string{"Warp Disruption Field Generator (Infinite Point / Bubble)"}
	case "Force Recon Ship":
		return []string{"Covert Ops Cloak", "Covert Cynosural Field", "Cynosural Field"}
	case "Combat Recon Ship":
		return []string{"Not Shown on Directional Scan"}
	case "Logistics", "Logistics Frigate":
		return []string{"Remote Repairs"}
	case "Command Ship", "Command Destroyer":
		return []string{"Command Bursts"}
	case "Strategic Cruiser":
		return []string{"Subsystem Dependent (T3C)", "Covert Cloak Subgroup Possible"}
	case "Stealth Bomber":
		return []string{"Covert Ops Cloak", "Bomb Launcher"}
	case "Interceptor":
		return []string{"Immune to Interdiction Bubbles"}
	case "Titan", "Supercarrier", "Carrier", "Dreadnought", "Lancer Dreadnought", "Force Auxiliary":
		return []string{"Capital Hull"}
	}
	return nil
}

// --- View ------------------------------------------------------------------

type wbRole struct {
	Slug, Label string
	N           int
}

type wbInstance struct {
	Name string // the item's own name from the paste
	Dist string
	On   bool // on grid (a row with no distance shown reads as present)
}

type wbHull struct {
	TypeID  int64
	Name    string
	Class   string // SDE group
	Sub     string // class, then the first item name the paste showed
	Role    string // chip label
	RoleKey string
	N       int
	Grid    int // of N, how many on grid
	Range   string
	// Instances are the paste's own rows of this type, for the
	// drawer.
	Instances []wbInstance
}

type wbClassRow struct {
	Class string
	Names string
	Role  string
	N     int
	Grid  int
	Off   int
}

type wbPilot struct {
	ID         int64
	Name       string
	CorpLine   string // Corp [TICK] - Alliance, "" when not stored
	Bucket     string // hostile | friendly | neutral | unknown
	Sec        string // dossier security status, "" when none stored
	Age        string // dossier age ("6.6y", "19d old"), "" when none stored
	History    string // "6 corps in history", "" when none stored
	Young      bool   // dossier-backed: under 90 days old
	Recent     bool   // dossier-backed: joined this corp within 7 days
	KilledHere bool   // the roster reading's own sighting: killed in this system this week
	Kills      string // sighting line from the reading, "" when none
	Facts      string // sec, age, history and kills, joined for the row's meta line
	HasDrawer  bool   // a dossier drawer was built for this pilot
}

type wbAlliance struct {
	Name     string
	Badge    string // the majority standing of its pilots, "" when mixed
	BadgeKey string
	CorpLine string // "3 Corp(s): A [X], B [Y]"
	NPilots  int
	Young    int // dossier-backed counts, complete or not shown
	Recent   int
	FlagsOK  bool
	Pilots   []wbPilot
}

type wbDelta struct {
	Name string
	N    int    // how many changed hands
	Dir  string // "added" | "left" | "grid"
	Text string // the row's right-hand figure
}

type wbPilotDelta struct {
	ID     int64
	Name   string
	Dir    string // "joined" | "left"
	Bucket string // standing word for a joined pilot
}

type wbGate struct {
	SystemID int64
	Name     string
	SecText  string
	SecTier  int
	Class    string // Highsec | Lowsec | Nullsec
	Region   string
	HasStats bool
	Jumps    int64
	Ship     int64
	Pod      int64
}

type wbHullDrawer struct {
	TypeID    int64
	Title     string
	Class     string
	Slots     string
	Warp      string
	Mass      string
	Sensor    string
	Role      string
	Caps      []string
	Desc      string
	Instances []wbInstance
}

type wbPilotDrawer struct {
	ID       int64
	Name     string
	AgeLine  string // "2410 days (6.6y)"
	Sec      string
	Standing string
	Warn     string // the young / recent-join note, "" when neither
	History  []pilotHistoryView
	Sighting string
}

type workbenchView struct {
	Error string

	ReportID    int64
	ChannelID   int64
	ChannelName string
	CanPost     bool
	SystemID    int64

	// The scan door: a standalone scan opens this same page,
	// driven by its own stored reading. FromScan marks that
	// door; ScanID is the scan's id either way (a report's page
	// is driven by the scan it carries, when it carries one).
	FromScan bool
	ScanID   string
	ScanURL  string // the classic scan page, scan door only

	// Header snapshot.
	Snapshot string // "HED-GP Gatecamp (Catch)"
	SysName  string
	Ago      string
	Headline string
	Reporter string
	Hulls    int
	PilotsN  int

	// The group picture: set when the report's group has more
	// than one member and the page renders the stored
	// composite. Rail lists the members; RailKinds summarises
	// them by kind for the rail's header.
	Grouped   bool
	GroupID   int64
	Rail      []wbRailItem
	RailMore  int // members beyond the rail's cap
	RailKinds string

	// Box 1: grid & range split.
	HasDScan   bool
	OnGrid     int
	OffGrid    int
	Deploy     int
	PointBlank int
	BarOn      int
	BarOff     int
	BarDep     int

	// Box 2: tactical hull roles.
	Roles    []wbRole
	Doctrine string

	// Box 3: local & standings.
	HasCounts   bool
	Hostile     int
	Neutral     int
	Friendly    int
	Unclass     int
	YoungAlts   int
	RecentJoins int
	FlagsKnown  bool

	// Box 4: system context.
	SecText  string
	SecTier  int
	SysLine  string // "Catch - Nullsec"
	HasStats bool
	Jumps1h  int64
	ShipPod  string // "27 / 14"
	NPC1h    int64
	Sov      string

	// D-scan panel.
	Objects int
	Types   []wbHull // aggregated by type (ships, then deployables)
	Rows    []wbHull // individual rows, closest first
	Classes []wbClassRow

	// Local panel.
	Alliances []wbAlliance
	AllPilots []wbPilot
	Unknown   []string
	NoLocal   bool // neither a roster reading nor named pilots

	// Diff view.
	HasPrev     bool
	PrevAgo     string
	PrevBy      string
	DiffGroup   bool // the other side is the group's previous composite
	HullDeltas  []wbDelta
	PilotDeltas []wbPilotDelta

	// System view.
	Gates []wbGate

	// Drawers.
	HullDrawers  []wbHullDrawer
	PilotDrawers []wbPilotDrawer
}

// --- Handler ---------------------------------------------------------------

func (app *Application) handleWorkbench(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	rid, _ := strconv.ParseInt(chi.URLParam(r, "reportID"), 10, 64)
	id := app.intelIdentity(ctx)
	rep, err := app.queries.GetIntelReportFull(ctx, rid)
	c, access := accessChannel{}, false
	if err == nil {
		c, access = app.channelAccess(ctx, id, rep.ChannelID)
	}
	if err != nil || !access {
		data.Workbench = &workbenchView{Error: "This report does not exist, or you have no access to its channel."}
		app.render(ctx, w, http.StatusNotFound, "intel_workbench.html", data)
		return
	}
	v := app.buildWorkbench(ctx, rep, c)
	data.Workbench = v
	title := v.SysName
	if title == "" {
		title = "Report"
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title + " · Intel Workbench", CanonicalPath: fmt.Sprintf("/intel/workbench/%d/", rep.ID)})
	app.render(ctx, w, http.StatusOK, "intel_workbench.html", data)
}

// buildWorkbench assembles the page from the report's stored rows.
func (app *Application) buildWorkbench(ctx context.Context, rep db.IntelReport, c accessChannel) *workbenchView {
	names := app.newKillNames(ctx)
	v := &workbenchView{
		ReportID: rep.ID, ChannelID: rep.ChannelID, ChannelName: c.Name,
		CanPost: c.CanPost, SystemID: rep.SystemID, ScanID: rep.ScanID,
		Headline: rep.Text, Reporter: rep.Reporter,
		Ago: ageText(time.Now(), rep.SeenAt),
	}
	sys := names.system(rep.SystemID)
	v.SysName = sys.Name
	v.SecText, v.SecTier = secRound(sys.Security)
	region := names.region(sys.RegionID)
	if region != "" {
		v.SysLine = region + " · " + secClassWord(sys.Security)
	}
	// The snapshot's name: system, what the report is, region.
	label := intelKindLabels[rep.Kind]
	if label == "" {
		label = "Report"
	}
	v.Snapshot = sys.Name + " " + label
	if region != "" {
		v.Snapshot += " (" + region + ")"
	}

	var analysis intelanalyze.Analysis
	if rep.Analysis != "" {
		_ = json.Unmarshal([]byte(rep.Analysis), &analysis)
	}
	if analysis.Counts != nil {
		v.HasCounts = true
		v.Hostile, v.Friendly = analysis.Counts.Hostile, analysis.Counts.Friendly
		v.Neutral, v.Unclass = analysis.Counts.Neutral, analysis.Counts.Unclassified
	}

	// The stored reading behind the report, and the paste it was
	// made from (kept with the scan): the reading gives the
	// tallies, the paste gives back each row's own distance.
	var res scanResult
	var rawRows []scan.DScanRow
	if rep.ScanID != "" {
		if row, err := app.queries.GetScan(ctx, rep.ScanID); err == nil {
			_ = json.Unmarshal([]byte(row.Result), &res)
			if p, err := scan.Parse(row.Raw); err == nil && p.Kind == scan.KindDScan {
				rawRows = p.DScan
			}
		}
	}

	// The group picture: when the report's group has more than
	// one member, the page renders the stored composite (the
	// newest d-scan governing, the locals merged) and the diff
	// runs against the previous composite. A singleton renders
	// exactly as it always has, from its own stored rows.
	var comp, prevComp *intelComposite
	grouped := false
	if rep.GroupID > 0 {
		var ok bool
		comp, prevComp, ok = app.loadGroupComposite(ctx, rep.GroupID)
		grouped = ok && len(comp.Members) > 1
	}
	fillAnalysis := &analysis
	dscan, dscanRows, local := res.DScan, rawRows, res.Local
	if grouped {
		now := time.Now()
		v.Grouped = true
		v.GroupID = rep.GroupID
		if comp.Headline != "" {
			v.Headline = comp.Headline
		}
		if !comp.LatestAt.IsZero() {
			v.Ago = ageText(now, comp.LatestAt)
		}
		v.Rail, v.RailMore = wbRail(now, comp, rep.ID)
		v.RailKinds = wbRailKinds(comp)
		if comp.Counts != nil {
			v.HasCounts = true
			v.Hostile, v.Friendly = comp.Counts.Hostile, comp.Counts.Friendly
			v.Neutral, v.Unclass = comp.Counts.Neutral, comp.Counts.Unclassified
		} else if comp.Local != nil {
			v.HasCounts = false // wbFillLocal recounts from the union
		}
		if comp.DScan != nil {
			dscan, dscanRows = comp.DScan, comp.DScanRows
		}
		local = comp.Local
		fillAnalysis = &intelanalyze.Analysis{Pilots: comp.Pilots, Ships: comp.Ships, Counts: comp.Counts}
	}

	if dscan != nil {
		app.wbFillDScan(ctx, v, dscan, dscanRows)
	}
	app.wbFillLocal(ctx, v, rep.ChannelID, rep.SystemID, fillAnalysis, local, names)
	if v.Hulls == 0 {
		for _, s := range fillAnalysis.Ships {
			n := s.N
			if n == 0 {
				n = 1
			}
			v.Hulls += n
		}
	}
	app.wbFillSystem(ctx, v, rep.SystemID, names)
	if grouped {
		if prevComp != nil {
			v.HasPrev = true
			v.DiffGroup = true
			if n := len(prevComp.Members); n > 0 {
				last := prevComp.Members[n-1]
				v.PrevAgo = ageText(time.Now(), last.SeenAt)
				v.PrevBy = last.Reporter
			}
			curRes := &scanResult{DScan: comp.DScan, Local: comp.Local}
			prevRes := &scanResult{DScan: prevComp.DScan, Local: prevComp.Local}
			prevA := &intelanalyze.Analysis{Pilots: prevComp.Pilots, Ships: prevComp.Ships}
			app.wbFillDiffSets(v, curRes, fillAnalysis, prevRes, prevA)
		}
	} else {
		app.wbFillDiff(ctx, v, rep, &analysis, &res)
	}
	return v
}

// --- The scan door (built, not yet routed) ----------------------------------

// handleWorkbenchScan is the workbench's second door: a
// standalone scan from the scan tool opened in the workbench,
// its own stored reading driving the page exactly as a
// report's page is driven by the report's reading. It is built
// but deliberately unrouted — no route serves it and nothing
// links to it, so the scan tool keeps its own result page.
// Switching the door on is: register this handler beside the
// scan routes, and land a scan submission here instead of on
// the classic scan page.
func (app *Application) handleWorkbenchScan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	w.Header().Set("X-Robots-Tag", "noindex")
	scanID := chi.URLParam(r, "scanID")
	var row db.GetScanRow
	var err error
	if validScanID(scanID) {
		row, err = app.queries.GetScan(ctx, scanID)
	} else {
		err = errors.New("bad id")
	}
	if err != nil {
		data.Workbench = &workbenchView{Error: "This scan does not exist, or it is more than 90 days old."}
		app.render(ctx, w, http.StatusNotFound, "intel_workbench.html", data)
		return
	}
	v := app.buildWorkbenchScan(ctx, row)
	data.Workbench = v
	title := v.SysName
	if title == "" {
		title = "Scan"
	}
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: title + " · Intel Workbench", CanonicalPath: "/intel/workbench/scan/" + row.ID + "/"})
	app.render(ctx, w, http.StatusOK, "intel_workbench.html", data)
}

// buildWorkbenchScan assembles the workbench from a standalone
// scan's stored reading: the same fills a report's page gets,
// minus the channel's frame (no channel, no reporter, no
// previous report to diff against — a scan has none of those).
// Standing buckets are read with no channel's perspective: the
// watch list and side are empty, so a pilot reads as their
// affiliation alone says.
func (app *Application) buildWorkbenchScan(ctx context.Context, row db.GetScanRow) *workbenchView {
	names := app.newKillNames(ctx)
	var res scanResult
	_ = json.Unmarshal([]byte(row.Result), &res)
	systemID := row.SystemID
	if systemID == 0 {
		systemID = res.SystemID
	}
	v := &workbenchView{
		FromScan: true, ScanID: row.ID, ScanURL: "/intel/scan/" + row.ID + "/",
		SystemID: systemID, Headline: scanTitle(res),
		Ago: ageText(time.Now(), row.CreatedAt),
	}
	label := wbScanKindLabel(res.Kind)
	v.Snapshot = label
	if systemID > 0 {
		sys := names.system(systemID)
		v.SysName = sys.Name
		v.SecText, v.SecTier = secRound(sys.Security)
		region := names.region(sys.RegionID)
		if sys.Name != "" {
			v.Snapshot = sys.Name + " " + label
		}
		if region != "" {
			v.SysLine = region + " · " + secClassWord(sys.Security)
			v.Snapshot += " (" + region + ")"
		}
	}
	var rawRows []scan.DScanRow
	if p, err := scan.Parse(row.Raw); err == nil && p.Kind == scan.KindDScan {
		rawRows = p.DScan
	}
	if res.DScan != nil {
		app.wbFillDScan(ctx, v, res.DScan, rawRows)
	}
	var noAnalysis intelanalyze.Analysis
	app.wbFillLocal(ctx, v, 0, systemID, &noAnalysis, res.Local, names)
	if systemID > 0 {
		app.wbFillSystem(ctx, v, systemID, names)
	}
	return v
}

// wbScanKindLabel names a scan's kind the way the page does.
func wbScanKindLabel(k scan.Kind) string {
	switch k {
	case scan.KindDScan:
		return "D-Scan"
	case scan.KindLocal:
		return "Local"
	case scan.KindFleet:
		return "Fleet"
	case scan.KindProbe:
		return "Probe Scan"
	}
	return "Scan"
}

// secClassWord names a security band the way the design does.
func secClassWord(sec float64) string {
	switch {
	case sec >= 0.5:
		return "Highsec"
	case sec > 0.0:
		return "Lowsec"
	default:
		return "Nullsec"
	}
}

// wbDist words a d-scan distance the way the design does: metres
// under a kilometre, kilometres under an AU, then AU.
func wbDist(km float64) string {
	switch {
	case km < 0:
		return "-"
	case km < 1:
		return strconv.FormatFloat(km*1000, 'f', 0, 64) + " m"
	case km < scan.AUKm:
		return strconv.FormatFloat(km, 'f', 0, 64) + " km"
	default:
		return strconv.FormatFloat(km/scan.AUKm, 'f', 1, 64) + " AU"
	}
}

// --- Box 1 & 2, the d-scan panel -------------------------------------------

// wbTypeRows groups the paste's rows by type: each type's own
// rows, its distance span, and how many sit point-blank.
type wbTypeRows struct {
	byType     map[int64][]scan.DScanRow
	pointBlank int
}

func wbGroupRows(rows []scan.DScanRow) *wbTypeRows {
	g := &wbTypeRows{byType: map[int64][]scan.DScanRow{}}
	for _, r := range rows {
		g.byType[r.TypeID] = append(g.byType[r.TypeID], r)
		if r.DistKm >= 0 && r.DistKm < 50 {
			g.pointBlank++
		}
	}
	return g
}

func (g *wbTypeRows) instances(typeID int64) []wbInstance {
	rows := g.byType[typeID]
	out := make([]wbInstance, 0, len(rows))
	for _, r := range rows {
		out = append(out, wbInstance{Name: r.Name, Dist: wbDist(r.DistKm), On: r.DistKm < 0 || r.DistKm <= scanGridKm})
	}
	return out
}

func (g *wbTypeRows) rangeText(typeID int64) string {
	rows := g.byType[typeID]
	lo, hi, any := 0.0, 0.0, false
	for _, r := range rows {
		if r.DistKm < 0 {
			continue
		}
		if !any || r.DistKm < lo {
			lo = r.DistKm
		}
		if !any || r.DistKm > hi {
			hi = r.DistKm
		}
		any = true
	}
	if !any {
		return ""
	}
	if hi-lo < 0.5 {
		return wbDist(lo)
	}
	return wbDist(lo) + " – " + wbDist(hi)
}

func (g *wbTypeRows) firstName(typeID int64) string {
	if rows := g.byType[typeID]; len(rows) > 0 {
		return rows[0].Name
	}
	return ""
}

// wbFillDScan fills box 1, box 2 and the d-scan panel from the
// stored reading and its paste.
func (app *Application) wbFillDScan(ctx context.Context, v *workbenchView, d *dscanReading, rawRows []scan.DScanRow) {
	v.HasDScan = true
	v.OnGrid, v.OffGrid = d.ShipsGrid, d.Ships-d.ShipsGrid
	for _, c := range d.Deployables {
		v.Deploy += c.N
	}
	v.Hulls = d.Ships
	v.Objects = d.Ships + v.Deploy
	groups := wbGroupRows(rawRows)
	v.PointBlank = groups.pointBlank
	total := v.OnGrid + v.OffGrid + v.Deploy
	if total > 0 {
		v.BarOn = v.OnGrid * 100 / total
		v.BarOff = v.OffGrid * 100 / total
		v.BarDep = v.Deploy * 100 / total
	}

	// Box 2: roles over the ship types, deployables as their own.
	roleN := map[string]int{}
	for _, c := range d.Types {
		roleN[wbRoleOf(c.Class)] += c.N
	}
	if v.Deploy > 0 {
		roleN["probes"] = v.Deploy
	}
	for _, slug := range wbRoleOrder {
		if roleN[slug] > 0 {
			v.Roles = append(v.Roles, wbRole{Slug: slug, Label: wbRoleLabel[slug], N: roleN[slug]})
		}
	}
	var doc []string
	if roleN["dictor"] > 0 {
		doc = append(doc, "Bubble Camp")
	}
	if roleN["capital"] > 0 {
		doc = append(doc, fmt.Sprintf("%d Capital(s)", roleN["capital"]))
	}
	if roleN["recon"] > 0 {
		doc = append(doc, "Cyno")
	}
	v.Doctrine = strings.Join(doc, " + ")

	// The aggregated table: ship types in the reading's own
	// order, then the deployables.
	mk := func(c scanCount, probes bool) wbHull {
		role := wbRoleOf(c.Class)
		if probes {
			role = "probes"
		}
		h := wbHull{TypeID: c.ID, Name: c.Name, Class: c.Class, Role: wbRoleChip[role], RoleKey: role,
			N: c.N, Grid: c.Grid, Range: groups.rangeText(c.ID), Instances: groups.instances(c.ID)}
		h.Sub = c.Class
		if first := groups.firstName(c.ID); first != "" && first != c.Name {
			h.Sub += " · \u201c" + first + "\u201d"
		}
		return h
	}
	for _, c := range d.Types {
		v.Types = append(v.Types, mk(c, false))
	}
	for _, c := range d.Deployables {
		v.Types = append(v.Types, mk(c, true))
	}

	// Individual rows: the paste's ship and deployable rows,
	// closest first, a row the client showed no distance for
	// last.
	shipOrDep := map[int64]bool{}
	byID := map[int64]wbHull{}
	for _, h := range v.Types {
		shipOrDep[h.TypeID] = true
		byID[h.TypeID] = h
	}
	sorted := append([]scan.DScanRow(nil), rawRows...)
	sort.SliceStable(sorted, func(i, j int) bool {
		di, dj := sorted[i].DistKm, sorted[j].DistKm
		if di < 0 {
			di = 1 << 40
		}
		if dj < 0 {
			dj = 1 << 40
		}
		return di < dj
	})
	for _, r := range sorted {
		if !shipOrDep[r.TypeID] {
			continue
		}
		base := byID[r.TypeID]
		row := wbHull{TypeID: r.TypeID, Name: r.TypeName, Class: base.Class, Role: base.Role, RoleKey: base.RoleKey,
			N: 1, Range: wbDist(r.DistKm)}
		if r.DistKm >= 0 && r.DistKm <= scanGridKm {
			row.Grid = 1
		}
		row.Sub = base.Class
		if r.Name != "" && r.Name != r.TypeName {
			row.Sub += " · \u201c" + r.Name + "\u201d"
		}
		v.Rows = append(v.Rows, row)
	}

	// By class: the table's types folded by SDE group, so the
	// three modes agree with each other.
	classN := map[string]*wbClassRow{}
	var classOrder []string
	for _, h := range v.Types {
		cr := classN[h.Class]
		if cr == nil {
			cr = &wbClassRow{Class: h.Class, Role: h.Role}
			classN[h.Class] = cr
			classOrder = append(classOrder, h.Class)
		}
		cr.N += h.N
		cr.Grid += h.Grid
		if cr.Names == "" {
			cr.Names = h.Name
		} else if !strings.Contains(cr.Names, h.Name) {
			cr.Names += ", " + h.Name
		}
	}
	for _, name := range classOrder {
		cr := classN[name]
		cr.Off = cr.N - cr.Grid
		v.Classes = append(v.Classes, *cr)
	}
	sort.SliceStable(v.Classes, func(i, j int) bool {
		if v.Classes[i].N != v.Classes[j].N {
			return v.Classes[i].N > v.Classes[j].N
		}
		return v.Classes[i].Class < v.Classes[j].Class
	})

	app.wbFillHullDrawers(ctx, v)
}

// wbFillHullDrawers builds each table type's drawer from stored
// data: slot layout, warp, mass and sensor from the type's
// stored dogma attributes (one batched read), its SDE
// description, and its instances in this scan.
func (app *Application) wbFillHullDrawers(ctx context.Context, v *workbenchView) {
	if len(v.Types) == 0 {
		return
	}
	ids := make([]int64, 0, len(v.Types))
	for _, h := range v.Types {
		ids = append(ids, h.TypeID)
	}
	attrs := map[int64]map[int64]float64{}
	if rows, err := app.queries.ListSDETypeAttributesByIDs(ctx, ids); err == nil {
		for _, r := range rows {
			if attrs[r.TypeID] == nil {
				attrs[r.TypeID] = map[int64]float64{}
			}
			attrs[r.TypeID][r.AttributeID] = r.Value
		}
	} else {
		logging.Warnf("workbench: type attributes: %v", err)
	}
	for _, h := range v.Types {
		d := wbHullDrawer{TypeID: h.TypeID, Title: h.Name, Class: h.Class, Role: h.Role,
			Caps: wbCaps(h.Class), Instances: h.Instances}
		if a := attrs[h.TypeID]; a != nil {
			hi, med, low, rig := int(a[14]), int(a[13]), int(a[12]), int(a[1137])
			if hi+med+low+rig > 0 {
				d.Slots = fmt.Sprintf("%dH / %dM / %dL / %dR", hi, med, low, rig)
			}
			if w := a[600]; w > 0 {
				d.Warp = strconv.FormatFloat(3*w, 'f', 1, 64) + " AU/s"
			}
			if m := a[4]; m > 0 {
				d.Mass = wbKg(m)
			}
			for _, s := range []struct {
				id   int64
				name string
			}{{211, "Gravimetric"}, {210, "Magnetometric"}, {209, "Ladar"}, {208, "Radar"}} {
				if sv := a[s.id]; sv > 0 {
					d.Sensor = fmt.Sprintf("%s (%d)", s.name, int(sv))
					break
				}
			}
		}
		if t, err := app.queries.GetSDEType(ctx, h.TypeID); err == nil {
			d.Desc = strings.TrimSpace(t.Description)
		}
		v.HullDrawers = append(v.HullDrawers, d)
	}
}

// wbKg words a mass in kilograms with thousands separators.
func wbKg(m float64) string {
	s := strconv.FormatFloat(m, 'f', 0, 64)
	var out strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(r)
	}
	return out.String() + " kg"
}

// --- Box 3, the local panel, pilot drawers ---------------------------------

// wbFacts joins a pilot row's stored facts into its meta line.
func wbFacts(p *wbPilot) string {
	var parts []string
	if p.Sec != "" {
		parts = append(parts, "sec "+p.Sec)
	}
	if p.Age != "" {
		parts = append(parts, p.Age)
	}
	if p.History != "" {
		parts = append(parts, p.History)
	}
	if p.Kills != "" {
		parts = append(parts, p.Kills)
	}
	return strings.Join(parts, " · ")
}

// wbBucketWord names a standing bucket the way the page speaks
// of it; the buckets are channelStanding's.
func wbBucketWord(b int) string {
	switch b {
	case bucketHostile:
		return "hostile"
	case bucketFriendly:
		return "friendly"
	case bucketNeutral:
		return "neutral"
	}
	return "unknown"
}

func wbBucketTitle(b string) string {
	switch b {
	case "hostile":
		return "Hostile"
	case "friendly":
		return "Friendly"
	case "neutral":
		return "Neutral"
	}
	return "Unclassified"
}

// wbDossier is one pilot's settled record, read for the page.
type wbDossier struct {
	payload  pilotPayload
	ageDays  int
	sec      float64
	ageOK    bool
	joinDays int // days in the current corporation, -1 when unknown
}

// wbDossiers reads the settled pilot records of ids in one
// bounded pass; pilots without a settled record are absent.
func (app *Application) wbDossiers(ctx context.Context, ids []int64) map[int64]*wbDossier {
	out := map[int64]*wbDossier{}
	if len(ids) == 0 {
		return out
	}
	if len(ids) > wbDossierCap {
		ids = ids[:wbDossierCap]
	}
	rows, err := app.queries.ListPilotRecordsByIDs(ctx, ids)
	if err != nil {
		logging.Warnf("workbench: pilot records: %v", err)
		return out
	}
	now := time.Now()
	for _, r := range rows {
		var p pilotPayload
		if json.Unmarshal([]byte(r.Payload), &p) != nil || p.Profile.Name == "" {
			continue
		}
		d := &wbDossier{payload: p, sec: p.Profile.SecurityStatus, joinDays: -1}
		if t, err := time.Parse(time.RFC3339, p.Profile.Birthday); err == nil {
			d.ageDays = int(now.Sub(t).Hours() / 24)
			d.ageOK = true
		}
		if n := len(p.History); n > 0 {
			if t, err := time.Parse(time.RFC3339, p.History[n-1].Start); err == nil {
				d.joinDays = int(now.Sub(t).Hours() / 24)
			}
		}
		out[r.CharacterID] = d
	}
	return out
}

// wbAge words a dossier age the way the roster shows it.
func wbAge(days int) string {
	if days < 365 {
		return fmt.Sprintf("%dd old", days)
	}
	return strconv.FormatFloat(float64(days)/365.25, 'f', 1, 64) + "y"
}

// wbFillLocal fills box 3's roster side, the local panel and the
// pilot drawers: from the report's roster reading when it has
// one, else from the pilots its analysis named.
func (app *Application) wbFillLocal(ctx context.Context, v *workbenchView, channelID, systemID int64, analysis *intelanalyze.Analysis, local *localReading, names *killNames) {
	now := time.Now()
	if local != nil && len(local.Pilots) > 0 {
		v.PilotsN = len(local.Pilots) + len(local.Unknown)
		v.Unknown = local.Unknown
		st := app.channelStandingOf(ctx, channelID)
		ids := make([]int64, 0, len(local.Pilots))
		for _, lp := range local.Pilots {
			ids = append(ids, lp.ID)
		}
		dossiers := app.wbDossiers(ctx, ids)
		// Names the reading itself stores, group by group.
		corpName, alliName := map[int64]string{}, map[int64]string{}
		for _, g := range local.Alliances {
			alliName[g.ID] = g.Name
			if g.Corp {
				corpName[g.ID] = g.Name
			}
			for _, c := range g.Corps {
				corpName[c.ID] = c.Name
			}
		}
		buckets := map[int64]int{}
		counts := [4]int{}
		byID := map[int64]*wbPilot{}
		for _, lp := range local.Pilots {
			b := st.ofLocal(lp)
			buckets[lp.ID] = b
			counts[b]++
			p := &wbPilot{ID: lp.ID, Name: lp.Name, Bucket: wbBucketWord(b)}
			// Corp line from the reading, tickers from the
			// pilot's own settled record where it has one.
			cn := corpName[lp.CorpID]
			an := alliName[lp.AllianceID]
			d := dossiers[lp.ID]
			ct, at := "", ""
			if d != nil {
				if cn == "" {
					cn = d.payload.Corp.Name
				}
				ct = d.payload.Corp.Ticker
				at = d.payload.Alliance.Ticker
				if an == "" {
					an = d.payload.Alliance.Name
				}
			}
			if cn != "" {
				p.CorpLine = cn
				if ct != "" {
					p.CorpLine += " [" + ct + "]"
				}
				if an != "" {
					p.CorpLine += " • "
					if at != "" {
						p.CorpLine += at
					} else {
						p.CorpLine += an
					}
				}
			}
			if d != nil {
				p.Sec = strconv.FormatFloat(d.sec, 'f', 1, 64)
				if d.ageOK {
					p.Age = wbAge(d.ageDays)
					p.Young = d.ageDays < 90
				}
				if n := len(d.payload.History); n > 0 {
					p.History = fmt.Sprintf("%d corps in history", n)
				}
				p.Recent = d.joinDays >= 0 && d.joinDays <= 7
			}
			if lp.Kills > 0 {
				p.Kills = fmt.Sprintf("%d kills", lp.Kills)
				if lp.LastSystem > 0 {
					p.Kills += " · last in " + names.system(lp.LastSystem).Name
				}
			}
			p.KilledHere = lp.LastSystem == systemID && !lp.LastKill.IsZero() && now.Sub(lp.LastKill) <= 7*24*time.Hour
			p.Facts = wbFacts(p)
			byID[lp.ID] = p
			v.AllPilots = append(v.AllPilots, *p)
		}
		if !v.HasCounts {
			v.HasCounts = true
			v.Hostile, v.Friendly = counts[bucketHostile], counts[bucketFriendly]
			v.Neutral, v.Unclass = counts[bucketNeutral], counts[bucketUnknown]
		}
		// Dossier coverage decides the flag chips: a count over
		// a partial set would understate, so the chips show
		// only when every pilot's record is on file.
		if len(dossiers) == len(local.Pilots) {
			v.FlagsKnown = true
			for _, p := range v.AllPilots {
				if p.Young {
					v.YoungAlts++
				}
				if p.Recent {
					v.RecentJoins++
				}
			}
		}
		// Alliance sections, in the reading's own order; a
		// corporation outside any alliance is its own section,
		// and any pilot no group claims falls into a last one.
		grouped := map[int64]bool{}
		mkSection := func(name string, members []localPilot) {
			if len(members) == 0 {
				return
			}
			sec := wbAlliance{Name: name, NPilots: len(members)}
			bc := [4]int{}
			allDossiers := true
			corpsSeen := map[string]bool{}
			var corps []string
			for _, lp := range members {
				grouped[lp.ID] = true
				bc[buckets[lp.ID]]++
				p := byID[lp.ID]
				sec.Pilots = append(sec.Pilots, *p)
				if dossiers[lp.ID] == nil {
					allDossiers = false
				}
				if p.Young {
					sec.Young++
				}
				if p.Recent {
					sec.Recent++
				}
				if cn := corpName[lp.CorpID]; cn != "" && !corpsSeen[cn] {
					corpsSeen[cn] = true
					corps = append(corps, cn)
				}
			}
			sec.FlagsOK = allDossiers
			for b, word := range map[int]string{bucketHostile: "hostile", bucketFriendly: "friendly", bucketNeutral: "neutral"} {
				if bc[b]*2 > len(members) {
					sec.Badge, sec.BadgeKey = wbBucketTitle(word), word
				}
			}
			if len(corps) > 0 {
				shown := corps
				if len(shown) > 2 {
					shown = shown[:2]
				}
				sec.CorpLine = fmt.Sprintf("%d Corp(s): %s", len(corps), strings.Join(shown, ", "))
			}
			v.Alliances = append(v.Alliances, sec)
		}
		membersOf := func(g localGroup) []localPilot {
			var out []localPilot
			for _, lp := range local.Pilots {
				if g.Corp && lp.CorpID == g.ID && lp.AllianceID == 0 {
					out = append(out, lp)
				} else if !g.Corp && lp.AllianceID == g.ID {
					out = append(out, lp)
				}
			}
			return out
		}
		for _, g := range local.Alliances {
			name := g.Name
			if !g.Corp {
				// The alliance's ticker, from any member's record.
				for _, lp := range membersOf(g) {
					if d := dossiers[lp.ID]; d != nil && d.payload.Alliance.Ticker != "" {
						name += " [" + d.payload.Alliance.Ticker + "]"
						break
					}
				}
			}
			mkSection(name, membersOf(g))
		}
		var rest []localPilot
		for _, lp := range local.Pilots {
			if !grouped[lp.ID] {
				rest = append(rest, lp)
			}
		}
		if len(rest) > 0 {
			mkSection("Unallied / Independent", rest)
		}
		app.wbPilotDrawers(ctx, v, local.Pilots, dossiers, buckets, names)
		return
	}

	if analysis != nil && len(analysis.Pilots) > 0 {
		v.PilotsN = len(analysis.Pilots)
		ids := make([]int64, 0, len(analysis.Pilots))
		for _, p := range analysis.Pilots {
			ids = append(ids, p.ID)
		}
		dossiers := app.wbDossiers(ctx, ids)
		buckets := map[int64]int{}
		for _, ap := range analysis.Pilots {
			b := bucketUnknown
			if ap.Red {
				b = bucketHostile
			} else if ap.Friendly {
				b = bucketFriendly
			}
			buckets[ap.ID] = b
			p := wbPilot{ID: ap.ID, Name: ap.Name, Bucket: wbBucketWord(b)}
			if d := dossiers[ap.ID]; d != nil {
				if d.payload.Corp.Name != "" {
					p.CorpLine = d.payload.Corp.Name
					if d.payload.Corp.Ticker != "" {
						p.CorpLine += " [" + d.payload.Corp.Ticker + "]"
					}
					if d.payload.Alliance.Ticker != "" {
						p.CorpLine += " • " + d.payload.Alliance.Ticker
					}
				}
				p.Sec = strconv.FormatFloat(d.sec, 'f', 1, 64)
				if d.ageOK {
					p.Age = wbAge(d.ageDays)
					p.Young = d.ageDays < 90
				}
				if n := len(d.payload.History); n > 0 {
					p.History = fmt.Sprintf("%d corps in history", n)
				}
				p.Recent = d.joinDays >= 0 && d.joinDays <= 7
			}
			p.Facts = wbFacts(&p)
			v.AllPilots = append(v.AllPilots, p)
		}
		app.wbPilotDrawers(ctx, v, nil, dossiers, buckets, names)
		return
	}
	v.NoLocal = true
}

// wbPilotDrawers builds the dossier drawer for each pilot whose
// settled record is on file. sightings, when given, are the
// roster reading's own per-pilot kill sightings.
func (app *Application) wbPilotDrawers(ctx context.Context, v *workbenchView, sightings []localPilot, dossiers map[int64]*wbDossier, buckets map[int64]int, names *killNames) {
	sight := map[int64]localPilot{}
	for _, lp := range sightings {
		sight[lp.ID] = lp
	}
	now := time.Now()
	for i := range v.AllPilots {
		p := &v.AllPilots[i]
		d := dossiers[p.ID]
		if d == nil {
			continue
		}
		p.HasDrawer = true
		dr := wbPilotDrawer{ID: p.ID, Name: p.Name, Sec: strconv.FormatFloat(d.sec, 'f', 2, 64),
			Standing: wbBucketTitle(wbBucketWord(buckets[p.ID]))}
		if d.ageOK {
			dr.AgeLine = fmt.Sprintf("%d days old", d.ageDays)
			if d.ageDays >= 365 {
				dr.AgeLine = fmt.Sprintf("%d days (%s)", d.ageDays, wbAge(d.ageDays))
			}
		}
		young := d.ageOK && d.ageDays < 90
		recent := d.joinDays >= 0 && d.joinDays <= 7
		if young || recent {
			var parts []string
			if young {
				parts = append(parts, fmt.Sprintf("Character is %d days old", d.ageDays))
			}
			if recent && d.payload.Corp.Name != "" {
				parts = append(parts, fmt.Sprintf("joined %s %d days ago", d.payload.Corp.Name, d.joinDays))
			}
			if len(parts) > 0 {
				dr.Warn = strings.Join(parts, " and ") + ". Frequently indicates a dedicated Cyno, dictor, or eyes alt."
			}
		}
		for _, stint := range d.payload.History {
			row := pilotHistoryView{CorpID: stint.CorpID, CorpName: stint.CorpName, To: "Present"}
			if row.CorpName == "" {
				row.CorpName = "Unknown corporation"
				row.CorpID = 0
			}
			if t, err := time.Parse(time.RFC3339, stint.Start); err == nil {
				row.From = t.UTC().Format("2006-01-02")
			}
			if stint.End != "" {
				if t, err := time.Parse(time.RFC3339, stint.End); err == nil {
					row.To = t.UTC().Format("2006-01-02")
				}
			}
			dr.History = append(dr.History, row)
		}
		if lp, ok := sight[p.ID]; ok && lp.Kills > 0 {
			dr.Sighting = fmt.Sprintf("%d kills on the board", lp.Kills)
			if lp.LastSystem > 0 {
				dr.Sighting += " · last in " + names.system(lp.LastSystem).Name
			}
			if !lp.LastKill.IsZero() {
				dr.Sighting += " · " + ageText(now, lp.LastKill)
			}
		}
		v.PilotDrawers = append(v.PilotDrawers, dr)
	}
}

// --- Box 4 and the system view ----------------------------------------------

func (app *Application) wbFillSystem(ctx context.Context, v *workbenchView, systemID int64, names *killNames) {
	if stats, err := app.queries.GetSystemActivityStats(ctx, systemID); err == nil {
		v.HasStats = true
		v.Jumps1h = stats.Jumps1h
		v.ShipPod = fmt.Sprintf("%d / %d", stats.ShipKills1h, stats.PodKills1h)
		v.NPC1h = stats.NpcKills1h
	}
	if sov, err := app.queries.GetSystemSovereignty(ctx, systemID); err == nil {
		switch {
		case sov.AllianceID > 0:
			v.Sov = "Sov: " + names.alliance(sov.AllianceID)
		case sov.CorporationID > 0:
			v.Sov = "Sov: " + names.corp(sov.CorporationID)
		case sov.FactionID > 0 && sov.FactionName != "":
			v.Sov = "Sov: " + sov.FactionName
		}
	}
	neighbours, err := app.queries.ListSDESystemNeighbours(ctx, systemID)
	if err != nil || len(neighbours) == 0 {
		return
	}
	statsByID := map[int64]db.SystemActivityStat{}
	if rows, err := app.queries.ListSystemActivityStatsByIDs(ctx, neighbours); err == nil {
		for _, s := range rows {
			statsByID[s.SystemID] = s
		}
	}
	for _, id := range neighbours {
		sys := names.system(id)
		g := wbGate{SystemID: id, Name: sys.Name, Class: secClassWord(sys.Security), Region: names.region(sys.RegionID)}
		g.SecText, g.SecTier = secRound(sys.Security)
		if s, ok := statsByID[id]; ok {
			g.HasStats = true
			g.Jumps, g.Ship, g.Pod = s.Jumps1h, s.ShipKills1h, s.PodKills1h
		}
		v.Gates = append(v.Gates, g)
	}
	sort.SliceStable(v.Gates, func(i, j int) bool {
		if v.Gates[i].Jumps != v.Gates[j].Jumps {
			return v.Gates[i].Jumps > v.Gates[j].Jumps
		}
		return v.Gates[i].Name < v.Gates[j].Name
	})
}

// --- The diff view -----------------------------------------------------------

// wbTally is one side of the diff: how many of a hull, and how
// many of them on grid when the reading counted grids.
type wbTally struct {
	N, Grid int
	Known   bool // grid split known (a scan reading, not prose)
}

// wbTallies reads a report's hull tallies from its scan reading
// when it carries one, else from the ships its analysis named.
func wbTallies(res *scanResult, a *intelanalyze.Analysis) map[string]wbTally {
	out := map[string]wbTally{}
	if res != nil && res.DScan != nil {
		for _, c := range res.DScan.Types {
			out[c.Name] = wbTally{N: c.N, Grid: c.Grid, Known: true}
		}
		for _, c := range res.DScan.Deployables {
			out[c.Name] = wbTally{N: c.N, Grid: c.Grid, Known: true}
		}
		return out
	}
	if a != nil {
		for _, s := range a.Ships {
			n := s.N
			if n == 0 {
				n = 1
			}
			out[s.Name] = wbTally{N: n}
		}
	}
	return out
}

// wbRoster reads a report's pilot set: the roster reading's
// pilots, else the pilots its analysis named.
func wbRoster(res *scanResult, a *intelanalyze.Analysis) map[int64]string {
	out := map[int64]string{}
	if res != nil && res.Local != nil {
		for _, lp := range res.Local.Pilots {
			out[lp.ID] = lp.Name
		}
		return out
	}
	if a != nil {
		for _, p := range a.Pilots {
			out[p.ID] = p.Name
		}
	}
	return out
}

// wbReadStored loads a report's analysis and scan reading, as
// buildWorkbench loads the current report's.
func (app *Application) wbReadStored(ctx context.Context, rep db.IntelReport) (intelanalyze.Analysis, scanResult) {
	var a intelanalyze.Analysis
	if rep.Analysis != "" {
		_ = json.Unmarshal([]byte(rep.Analysis), &a)
	}
	var res scanResult
	if rep.ScanID != "" {
		if row, err := app.queries.GetScan(ctx, rep.ScanID); err == nil {
			_ = json.Unmarshal([]byte(row.Result), &res)
		}
	}
	return a, res
}

func (app *Application) wbFillDiff(ctx context.Context, v *workbenchView, rep db.IntelReport, cur *intelanalyze.Analysis, curRes *scanResult) {
	prev, err := app.queries.GetPreviousIntelReport(ctx, db.GetPreviousIntelReportParams{
		ChannelID: rep.ChannelID, SystemID: rep.SystemID, BeforeAt: rep.SeenAt, BeforeID: rep.ID})
	if err != nil {
		return
	}
	v.HasPrev = true
	v.PrevAgo = ageText(time.Now(), prev.SeenAt)
	v.PrevBy = prev.Reporter
	prevA, prevRes := app.wbReadStored(ctx, prev)
	app.wbFillDiffSets(v, curRes, cur, &prevRes, &prevA)
}

// wbFillDiffSets computes the diff view's deltas between two
// readings: hull tallies and rosters, current against previous.
// The report diff calls it with the previous report's reading;
// the group diff calls it with the previous composite's faces.
func (app *Application) wbFillDiffSets(v *workbenchView, curRes *scanResult, cur *intelanalyze.Analysis, prevRes *scanResult, prev *intelanalyze.Analysis) {
	curT, prevT := wbTallies(curRes, cur), wbTallies(prevRes, prev)
	names := map[string]bool{}
	for n := range curT {
		names[n] = true
	}
	for n := range prevT {
		names[n] = true
	}
	var added, left, grid []wbDelta
	for n := range names {
		c, p := curT[n], prevT[n]
		switch {
		case c.N > p.N:
			d := wbDelta{Name: n, N: c.N - p.N, Dir: "added"}
			if c.Known {
				d.Text = fmt.Sprintf("%d on grid now", c.Grid)
			}
			added = append(added, d)
		case c.N < p.N:
			left = append(left, wbDelta{Name: n, N: p.N - c.N, Dir: "left"})
		case c.Known && p.Known && c.Grid != p.Grid:
			grid = append(grid, wbDelta{Name: n, Dir: "grid",
				Text: fmt.Sprintf("on grid %d → %d", p.Grid, c.Grid)})
		}
	}
	byN := func(ds []wbDelta) {
		sort.SliceStable(ds, func(i, j int) bool {
			if ds[i].N != ds[j].N {
				return ds[i].N > ds[j].N
			}
			return ds[i].Name < ds[j].Name
		})
	}
	byN(added)
	byN(left)
	v.HullDeltas = append(append(added, left...), grid...)

	curR, prevR := wbRoster(curRes, cur), wbRoster(prevRes, prev)
	buckets := map[int64]string{}
	for _, p := range v.AllPilots {
		buckets[p.ID] = p.Bucket
	}
	var joined, departed []wbPilotDelta
	for id, name := range curR {
		if _, ok := prevR[id]; !ok {
			joined = append(joined, wbPilotDelta{ID: id, Name: name, Dir: "joined", Bucket: wbBucketTitle(buckets[id])})
		}
	}
	for id, name := range prevR {
		if _, ok := curR[id]; !ok {
			departed = append(departed, wbPilotDelta{ID: id, Name: name, Dir: "left"})
		}
	}
	byName := func(ds []wbPilotDelta) {
		sort.SliceStable(ds, func(i, j int) bool { return ds[i].Name < ds[j].Name })
	}
	byName(joined)
	byName(departed)
	v.PilotDeltas = append(joined, departed...)
}
