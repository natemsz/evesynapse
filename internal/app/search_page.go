package app

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/app/module"
)

// The results page (GET /search/): what Enter in the top bar's
// search opens. The same local search as the bar's suggestions
// (searchAll), wider, one tab per kind of result with its count,
// and filters that fit the kind: systems by security band and
// region; items open the Item Database's own search, whose
// category, group and published-only filters are the items' ones.

// searchPageLimits are the results page's: enough to scan, and
// systems wide enough that the filters have something to narrow.
var searchPageLimits = searchLimits{characters: 20, items: 60, systems: 400, regions: 60, orgs: 60, pilots: 60}

const (
	// searchAllPerKind is how many of each kind the All tab lists
	// before its "show all" link.
	searchAllPerKind = 8
	// searchNameWait bounds how long the page waits on a name the
	// worker is looking up (searchAll noted it and nudged the
	// lookup awake): a moment, reading the want, never fetching.
	searchNameWait = 2500 * time.Millisecond
)

// searchKinds are the tabs, in order: key, label.
var searchKinds = [][2]string{
	{"all", "All"}, {"characters", "Your characters"}, {"items", "Items"}, {"pilots", "Pilots"},
	{"corporations", "Corporations"}, {"alliances", "Alliances"}, {"systems", "Systems"}, {"regions", "Regions"},
}

// searchSecBands are the systems filter's security bands.
var searchSecBands = [][2]string{{"high", "High sec"}, {"low", "Low sec"}, {"null", "Null sec"}, {"wh", "Wormhole space"}}

// searchSection is one kind's results on the page.
type searchSection struct {
	Key, Title string
	Hits       []searchHit
	Systems    []searchSystem
	Total      int
	// More links to the kind's own tab when the All tab cut it short.
	More string
}

type searchPageView struct {
	Query, Kind string
	Sections    []searchSection
	Total       int
	// Pending is a name still being looked up as a pilot.
	Pending bool
	// ItemsURL opens the Item Database's search for the query,
	// with its item filters.
	ItemsURL string
	// The systems filters: the band and region chosen, and the
	// regions the matching systems lie in.
	Sec      string
	SecBands [][2]string
	RegionID int64
	Regions  []searchHit
}

func (app *Application) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	q := r.URL.Query()
	view := &searchPageView{Query: searchQuery(r), Kind: q.Get("type"), Sec: q.Get("sec"), SecBands: searchSecBands}
	if !validSearchKind(view.Kind) {
		view.Kind = "all"
	}
	if !validSecBand(view.Sec) {
		view.Sec = ""
	}
	view.RegionID, _ = strconv.ParseInt(q.Get("region"), 10, 64)
	data.Search = view
	if len([]rune(view.Query)) < 2 {
		app.render(ctx, w, http.StatusOK, "search.html", data)
		return
	}
	view.ItemsURL = "/items/?" + url.Values{"q": {view.Query}}.Encode()

	userID := app.userID(ctx)
	set := app.searchAll(ctx, view.Query, userID, searchPageLimits)
	if set.Pending && app.awaitPilotName(ctx, view.Query) {
		set = app.searchAll(ctx, view.Query, userID, searchPageLimits)
	}
	view.Pending = set.Pending

	// Systems: the regions they lie in, for the region filter, then
	// the filters themselves.
	seen := map[int64]bool{}
	for _, s := range set.Systems {
		if s.RegionID > 0 && !seen[s.RegionID] {
			seen[s.RegionID] = true
			view.Regions = append(view.Regions, searchHit{ID: s.RegionID, Name: s.RegionName})
		}
	}
	sort.Slice(view.Regions, func(i, j int) bool { return view.Regions[i].Name < view.Regions[j].Name })
	if !seen[view.RegionID] {
		view.RegionID = 0
	}
	systems := set.Systems[:0:0]
	for _, s := range set.Systems {
		if (view.Sec == "" || secBand(s.Security, s.RegionID) == view.Sec) && (view.RegionID == 0 || s.RegionID == view.RegionID) {
			systems = append(systems, s)
		}
	}

	hits := map[string][]searchHit{
		"characters": set.Characters, "items": set.Items, "pilots": set.Pilots,
		"corporations": set.Corporations, "alliances": set.Alliances, "regions": set.Regions,
	}
	var tabs []module.NavLink
	counts := map[string]int{}
	for _, k := range searchKinds[1:] {
		n := len(hits[k[0]])
		if k[0] == "systems" {
			n = len(systems)
		}
		counts[k[0]] = n
		view.Total += n
	}
	for _, k := range searchKinds {
		n := counts[k[0]]
		if k[0] == "all" {
			n = view.Total
		}
		// A kind with nothing found has no tab, unless it is the
		// one open (a shared link, or filters that emptied it).
		if n == 0 && k[0] != "all" && k[0] != view.Kind {
			continue
		}
		if k[0] == "characters" && userID == 0 {
			continue
		}
		tabs = append(tabs, module.NavLink{Href: view.kindURL(k[0]), Label: k[1] + " " + strconv.Itoa(n), Active: k[0] == view.Kind})
	}
	data.SectionTabs = tabs

	for _, k := range searchKinds[1:] {
		if view.Kind != "all" && view.Kind != k[0] {
			continue
		}
		sec := searchSection{Key: k[0], Title: k[1], Total: counts[k[0]]}
		if k[0] == "systems" {
			sec.Systems = systems
		} else {
			sec.Hits = hits[k[0]]
		}
		if view.Kind == "all" {
			if sec.Total == 0 {
				continue
			}
			if sec.Total > searchAllPerKind {
				sec.More = view.kindURL(k[0])
				sec.Hits = capHits(sec.Hits, searchAllPerKind)
				if len(sec.Systems) > searchAllPerKind {
					sec.Systems = sec.Systems[:searchAllPerKind]
				}
			}
		}
		view.Sections = append(view.Sections, sec)
	}
	if userID > 0 {
		app.setTypingGuesses(ctx, app.typingBox(ctx, "topbar"), set.guesses, time.Now().UTC())
	}
	app.render(ctx, w, http.StatusOK, "search.html", data)
}

// awaitPilotName waits, briefly, for the worker to settle the name
// a search just noted, reading the want only. True when it settled.
func (app *Application) awaitPilotName(ctx context.Context, q string) bool {
	key := normalizePilotName(q)
	deadline := time.Now().Add(searchNameWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
		if want, err := app.queries.GetPilotNameWant(ctx, key); err == nil && (want.State == "ready" || want.State == "missing") {
			return true
		}
	}
	return false
}

// kindURL is the page for the query on one tab, keeping the
// systems filters only where they apply.
func (v *searchPageView) kindURL(kind string) string {
	p := url.Values{"q": {v.Query}}
	if kind != "all" {
		p.Set("type", kind)
	}
	if kind == "systems" || kind == "all" {
		if v.Sec != "" {
			p.Set("sec", v.Sec)
		}
		if v.RegionID > 0 {
			p.Set("region", strconv.FormatInt(v.RegionID, 10))
		}
	}
	return "/search/?" + p.Encode()
}

func validSearchKind(k string) bool {
	for _, s := range searchKinds {
		if s[0] == k {
			return true
		}
	}
	return false
}

func validSecBand(b string) bool {
	for _, s := range searchSecBands {
		if s[0] == b {
			return true
		}
	}
	return false
}

// secBand is the band a system's security puts it in, as the game
// rounds it: 0.5 and up is high sec, above 0.0 low, the rest null,
// and the wormhole regions (11000001 and up) wormhole space.
func secBand(sec float64, regionID int64) string {
	if regionID >= 11000000 && regionID < 12000000 {
		return "wh"
	}
	_, tier := secRound(sec)
	switch {
	case tier >= 5:
		return "high"
	case tier >= 1:
		return "low"
	}
	return "null"
}
