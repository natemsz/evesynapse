package app

// ---------------------------------------------------------------------------
// Scan compare (Step 3): /intel/scan/compare/?a=&b= diffs two
// stored readings of the same kind -- a d-scan's composition
// with its on/off-grid moves, a local's arrivals, departures
// and organization changes, a fleet's or probe scan's tallies.
// The diff reads exactly the two stored readings; A is "before"
// and B "after", and the page offers the swap.
// ---------------------------------------------------------------------------

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"evesynapse/internal/app/module"
)

// scanCompareView is the compare page: the form's two inputs
// and, when both sides load as the same kind, the diff.
type scanCompareView struct {
	AID, BID  string
	Error     string
	SwapURL   string
	KindLabel string
	// SystemMismatch: both readings name a system and they
	// differ -- the diff is still shown, under a warning.
	SystemMismatch bool
	A, B           *scanCompareSide
	DScan          *scanDScanDiff
	Local          *scanLocalDiff
	Tallies        []scanTallyDiff
}

// scanCompareSide is one reading as the compare page heads it.
type scanCompareSide struct {
	ID       string
	Title    string
	SysName  string
	SecText  string
	SecTier  int
	At       time.Time
	Ago      string
	Headline string
}

// scanTypeDiff is one tally row across two readings.
type scanTypeDiff struct {
	ID         int64
	Name       string
	Class      string
	Before     int
	After      int
	Delta      int
	DeltaText  string
	GridBefore int
	GridAfter  int
	State      string // "new", "gone", "" for present in both
	Move       string // "moved on grid", "moved off grid", ""
}

// signedText words a difference for the compare tables:
// "+7", "-3", an em dash when even.
func signedText(d int) string {
	switch {
	case d > 0:
		return "+" + itoa(d)
	case d < 0:
		return "-" + itoa(-d)
	}
	return "—"
}

// scanDScanDiff is two d-scans diffed.
type scanDScanDiff struct {
	ShipsBefore, ShipsAfter, ShipsDelta int
	ShipsDeltaText                      string
	GridBefore, GridAfter               int
	Types                               []scanTypeDiff
	Classes                             []scanTypeDiff
	Moves                               []scanTypeDiff
	NotableArrived, NotableLeft         []scanCount
	ValueBefore, ValueAfter             string // short ISK, "" when unpriced
}

// scanLocalDiff is two local readings diffed.
type scanLocalDiff struct {
	Arrived, Left []localPilot // capped for the page
	ArrivedN      int
	LeftN         int
	ArrivedMore   int // past the cap, for the "…and N more" line
	LeftMore      int
	StayedN       int
	Moved         []scanLocalMove
	Alliances     []scanOrgDiff
	Corps         []scanOrgDiff
	UnknownBefore int
	UnknownAfter  int
	PilotsBefore  int
	PilotsAfter   int
}

// scanLocalMove is one pilot who stayed but changed
// organization between the two readings.
type scanLocalMove struct {
	ID      int64
	Name    string
	FromOrg string
	ToOrg   string
}

// scanOrgDiff is one alliance or corporation across two
// local readings.
type scanOrgDiff struct {
	Name      string
	Before    int
	After     int
	Delta     int
	DeltaText string
}

// scanTallyDiff is one named tally list diffed (a fleet's
// classes, a probe scan's groups).
type scanTallyDiff struct {
	Title string
	Rows  []scanTypeDiff
}

// extractScanID pulls a scan id out of a pasted scan link or
// a bare id; "" when there is none.
func extractScanID(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "/intel/scan/"); i >= 0 {
		s = s[i+len("/intel/scan/"):]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if validScanID(s) {
		return s
	}
	return ""
}

// diffScanCounts unions two tally lists into before/after
// rows: counts, grid counts, what is new or gone, and the
// grid moves (a type whose count held while its on-grid
// count moved). Biggest change first.
func diffScanCounts(a, b []scanCount) []scanTypeDiff {
	key := func(c scanCount) string {
		if c.ID > 0 {
			return "i" + itoa(int(c.ID))
		}
		return "n" + c.Name
	}
	atA, atB := map[string]*scanCount{}, map[string]*scanCount{}
	var order []string
	seen := map[string]bool{}
	for i := range a {
		k := key(a[i])
		atA[k] = &a[i]
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	for i := range b {
		k := key(b[i])
		atB[k] = &b[i]
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	out := make([]scanTypeDiff, 0, len(order))
	for _, k := range order {
		var d scanTypeDiff
		if c := atA[k]; c != nil {
			d.ID, d.Name, d.Class = c.ID, c.Name, c.Class
			d.Before, d.GridBefore = c.N, c.Grid
		}
		if c := atB[k]; c != nil {
			d.ID, d.Name, d.Class = c.ID, c.Name, c.Class
			d.After, d.GridAfter = c.N, c.Grid
		}
		d.Delta = d.After - d.Before
		d.DeltaText = signedText(d.Delta)
		switch {
		case d.Before == 0:
			d.State = "new"
		case d.After == 0:
			d.State = "gone"
		case d.Delta == 0 && d.GridAfter > d.GridBefore:
			d.Move = "moved on grid"
		case d.Delta == 0 && d.GridAfter < d.GridBefore:
			d.Move = "moved off grid"
		}
		if d.Delta == 0 && d.GridAfter == d.GridBefore {
			continue // unchanged in every way the page shows
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].Delta, out[j].Delta
		if ai < 0 {
			ai = -ai
		}
		if aj < 0 {
			aj = -aj
		}
		if ai != aj {
			return ai > aj
		}
		if out[i].After != out[j].After {
			return out[i].After > out[j].After
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// notableDiff lists the notable hulls one d-scan has that
// the other does not, by hull name.
func notableDiff(a, b *dscanReading) (arrived, left []scanCount) {
	inA := map[string]bool{}
	for _, c := range a.Notable {
		inA[c.Name] = true
	}
	inB := map[string]bool{}
	for _, c := range b.Notable {
		inB[c.Name] = true
	}
	for _, c := range b.Notable {
		if !inA[c.Name] {
			arrived = append(arrived, c)
		}
	}
	for _, c := range a.Notable {
		if !inB[c.Name] {
			left = append(left, c)
		}
	}
	return arrived, left
}

// localOrgNames maps a local reading's organizations: every
// alliance and corporation id to its stored name.
func localOrgNames(l *localReading) map[int64]string {
	out := map[int64]string{}
	for _, g := range l.Alliances {
		out[g.ID] = g.Name
		for _, c := range g.Corps {
			out[c.ID] = c.Name
		}
	}
	return out
}

// localOrgLabel is a pilot's organization as a reading
// names it: their alliance, or their corporation when they
// fly without one.
func localOrgLabel(names map[int64]string, p localPilot) string {
	if p.AllianceID > 0 {
		if n := names[p.AllianceID]; n != "" {
			return n
		}
	}
	if n := names[p.CorpID]; n != "" {
		return n
	}
	return "Unknown"
}

// diffLocal diffs two local readings: who arrived, who
// left, who stayed and changed organization, and the
// per-organization count movement.
func diffLocal(a, b *localReading) *scanLocalDiff {
	d := &scanLocalDiff{
		UnknownBefore: len(a.Unknown), UnknownAfter: len(b.Unknown),
		PilotsBefore: len(a.Pilots), PilotsAfter: len(b.Pilots),
	}
	byIDA := map[int64]localPilot{}
	for _, p := range a.Pilots {
		byIDA[p.ID] = p
	}
	byIDB := map[int64]localPilot{}
	for _, p := range b.Pilots {
		byIDB[p.ID] = p
	}
	namesA, namesB := localOrgNames(a), localOrgNames(b)
	for _, p := range b.Pilots {
		old, ok := byIDA[p.ID]
		if !ok {
			d.Arrived = append(d.Arrived, p)
			continue
		}
		d.StayedN++
		if old.CorpID != p.CorpID || old.AllianceID != p.AllianceID {
			d.Moved = append(d.Moved, scanLocalMove{
				ID: p.ID, Name: p.Name, FromOrg: localOrgLabel(namesA, old), ToOrg: localOrgLabel(namesB, p),
			})
		}
	}
	for _, p := range a.Pilots {
		if _, ok := byIDB[p.ID]; !ok {
			d.Left = append(d.Left, p)
		}
	}
	sort.Slice(d.Arrived, func(i, j int) bool { return d.Arrived[i].Name < d.Arrived[j].Name })
	sort.Slice(d.Left, func(i, j int) bool { return d.Left[i].Name < d.Left[j].Name })
	sort.Slice(d.Moved, func(i, j int) bool { return d.Moved[i].Name < d.Moved[j].Name })
	d.ArrivedN, d.LeftN = len(d.Arrived), len(d.Left)
	if len(d.Arrived) > 100 {
		d.ArrivedMore = len(d.Arrived) - 100
		d.Arrived = d.Arrived[:100]
	}
	if len(d.Left) > 100 {
		d.LeftMore = len(d.Left) - 100
		d.Left = d.Left[:100]
	}
	d.Alliances = diffLocalGroups(a, b, false)
	d.Corps = diffLocalGroups(a, b, true)
	return d
}

// diffLocalGroups unions two readings' alliance rows (or
// corporation rows: nested corporations plus the standalone
// ones) into count movement, biggest movement first.
func diffLocalGroups(a, b *localReading, corps bool) []scanOrgDiff {
	rows := map[int64]*scanOrgDiff{}
	var order []int64
	add := func(l *localReading, before bool) {
		put := func(id int64, name string, n int) {
			r := rows[id]
			if r == nil {
				r = &scanOrgDiff{Name: name}
				rows[id] = r
				order = append(order, id)
			}
			if before {
				r.Before = n
			} else {
				r.After = n
			}
		}
		for _, g := range l.Alliances {
			if corps {
				if g.Corp {
					put(g.ID, g.Name, g.N)
				}
				for _, c := range g.Corps {
					put(c.ID, c.Name, c.N)
				}
			} else if !g.Corp {
				put(g.ID, g.Name, g.N)
			}
		}
	}
	add(a, true)
	add(b, false)
	out := make([]scanOrgDiff, 0, len(order))
	for _, id := range order {
		r := rows[id]
		r.Delta = r.After - r.Before
		if r.Delta == 0 {
			continue
		}
		r.DeltaText = signedText(r.Delta)
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := out[i].Delta, out[j].Delta
		if ai < 0 {
			ai = -ai
		}
		if aj < 0 {
			aj = -aj
		}
		if ai != aj {
			return ai > aj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// compareHeadline is one reading's counts line for the
// compare page's side headers.
func compareHeadline(res scanResult) string {
	switch {
	case res.DScan != nil:
		return plural(res.DScan.Ships, "ship") + " · " + itoa(res.DScan.ShipsGrid) + " on grid"
	case res.Local != nil:
		return plural(len(res.Local.Pilots)+len(res.Local.Unknown), "pilot")
	case res.Fleet != nil:
		return plural(len(res.Fleet.Members), "member")
	case res.Probe != nil:
		return plural(len(res.Probe.Sigs), "signature")
	}
	return ""
}

func (app *Application) handleScanCompare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	w.Header().Set("X-Robots-Tag", "noindex")
	v := &scanCompareView{AID: r.URL.Query().Get("a"), BID: r.URL.Query().Get("b")}
	data.ScanCompare = v
	data.HeadMeta = app.HeadMeta(module.PageMeta{Title: "Compare scans",
		Description:   "Diff two EveSynapse scans: composition changes, grid moves, arrivals and departures.",
		CanonicalPath: "/intel/scan/compare/"})
	load := func(raw string) (*scanCompareSide, scanResult, bool) {
		id := extractScanID(raw)
		if id == "" {
			return nil, scanResult{}, false
		}
		row, err := app.queries.GetScan(ctx, id)
		if err != nil {
			return nil, scanResult{}, false
		}
		var res scanResult
		if json.Unmarshal([]byte(row.Result), &res) != nil {
			return nil, scanResult{}, false
		}
		side := &scanCompareSide{ID: id, Title: scanTitle(res), At: row.CreatedAt,
			Ago: ageText(time.Now(), row.CreatedAt), Headline: compareHeadline(res)}
		if res.SystemID > 0 {
			names := app.newKillNames(ctx)
			sys := names.system(res.SystemID)
			side.SysName = sys.Name
			side.SecText, side.SecTier = secRound(sys.Security)
		}
		return side, res, true
	}
	if v.AID == "" && v.BID == "" {
		app.render(ctx, w, http.StatusOK, "scan_compare.html", data)
		return
	}
	aSide, aRes, aOK := load(v.AID)
	bSide, bRes, bOK := load(v.BID)
	switch {
	case !aOK || !bOK:
		v.Error = "One of those scans does not exist, or it is more than 90 days old."
	case aRes.Kind != bRes.Kind:
		v.Error = "Those are different kinds of scan — a comparison needs two of the same kind."
	default:
		v.A, v.B = aSide, bSide
		v.KindLabel = scanKindLabels[string(aRes.Kind)]
		v.SwapURL = "/intel/scan/compare/?a=" + bSide.ID + "&b=" + aSide.ID
		if aRes.SystemID > 0 && bRes.SystemID > 0 && aRes.SystemID != bRes.SystemID {
			v.SystemMismatch = true
		}
		switch {
		case aRes.DScan != nil:
			dd := &scanDScanDiff{
				ShipsBefore: aRes.DScan.Ships, ShipsAfter: bRes.DScan.Ships,
				ShipsDelta:     bRes.DScan.Ships - aRes.DScan.Ships,
				ShipsDeltaText: signedText(bRes.DScan.Ships - aRes.DScan.Ships),
				GridBefore:     aRes.DScan.ShipsGrid, GridAfter: bRes.DScan.ShipsGrid,
				Types:   diffScanCounts(aRes.DScan.Types, bRes.DScan.Types),
				Classes: diffScanCounts(aRes.DScan.Classes, bRes.DScan.Classes),
			}
			dd.NotableArrived, dd.NotableLeft = notableDiff(aRes.DScan, bRes.DScan)
			for _, t := range dd.Types {
				if t.Move != "" {
					dd.Moves = append(dd.Moves, t)
				}
			}
			if aRes.DScan.Value > 0 {
				dd.ValueBefore = formatISKShort(aRes.DScan.Value)
			}
			if bRes.DScan.Value > 0 {
				dd.ValueAfter = formatISKShort(bRes.DScan.Value)
			}
			v.DScan = dd
		case aRes.Local != nil:
			v.Local = diffLocal(aRes.Local, bRes.Local)
		case aRes.Fleet != nil:
			v.Tallies = []scanTallyDiff{
				{Title: "Ship classes", Rows: diffScanCounts(aRes.Fleet.Classes, bRes.Fleet.Classes)},
				{Title: "Ships", Rows: diffScanCounts(aRes.Fleet.Types, bRes.Fleet.Types)},
				{Title: "Systems", Rows: diffScanCounts(aRes.Fleet.Systems, bRes.Fleet.Systems)},
			}
		case aRes.Probe != nil:
			v.Tallies = []scanTallyDiff{
				{Title: "Signature types", Rows: diffScanCounts(aRes.Probe.Groups, bRes.Probe.Groups)},
			}
		}
	}
	app.render(ctx, w, http.StatusOK, "scan_compare.html", data)
}
