// Package intel is the Intel module: the Wars, Incursions and Faction
// Warfare pages. All of it is public ESI data, kept warm in the global
// store (global_snapshots + war_details) by the worker, so the pages
// read stored rows and local name caches only and never call out. The
// pages need no scope and no login.
package intel

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// Module is the Intel module's registration.
var Module = module.Module{
	ID:            "intel",
	Title:         "Intel",
	Requires:      []string{},
	Scopes:        []module.Scope{},
	EnhanceScopes: []module.Scope{},
	Activities:    []string{"market"},
	Nav: []module.NavSection{{
		Section: "intel",
		Entries: []module.NavEntry{
			// The killboard's pages are core (killboard.go): they
			// serve in every build, and lead this section's menu
			// when the section exists.
			{Href: "/intel/kills/", Label: "Killboard", Pages: []string{"killboard.html", "kill.html"}},
			{Href: "/intel/scan/", Label: "D-Scan Parser", Pages: []string{"scans.html", "scan.html", "scan_compare.html"}},
			{Href: "/intel/channels/", Label: "Intel Channels", Pages: []string{"channels.html", "channel.html"}},
			{Href: "/intel/workbench/", Label: "Intel Workbench", Pages: []string{"intel_dashboard.html", "intel_workbench.html"}},
			{Href: "/intel/feed/", Label: "Intel Feed", Pages: []string{"feed.html"}},
			{Href: "/intel/wars/", Label: "Wars", Pages: []string{"intel_wars.html"}},
			{Href: "/intel/incursions/", Label: "Incursions", Pages: []string{"intel_incursions.html"}},
			{Href: "/intel/fw/", Label: "Faction Warfare", Pages: []string{"intel_fw.html"}},
		},
	}},
	Pages: []module.Page{
		{Template: "intel_wars.html", Section: "intel"},
		{Template: "intel_incursions.html", Section: "intel"},
		{Template: "intel_fw.html", Section: "intel"},
	},
	Sitemap: []module.SitemapEntry{
		{Path: "/intel/wars/", ChangeFreq: "weekly", Priority: "0.7"},
		{Path: "/intel/incursions/", ChangeFreq: "weekly", Priority: "0.7"},
		{Path: "/intel/fw/", ChangeFreq: "weekly", Priority: "0.7"},
	},
	Routes: func(r chi.Router, deps module.Deps) {
		r.Route("/intel", func(r chi.Router) {
			r.Use(deps.AnonPages())
			r.Get("/wars/", handleWars(deps))
			r.Get("/incursions/", handleIncursions(deps))
			r.Get("/fw/", handleFW(deps))
		})
	},
}

// PageData is the view model of the three Intel templates; each page
// fills its own field.
type PageData struct {
	module.ShellData
	IntelWars       *warsView
	IntelIncursions *incursionsView
	IntelFW         *fwView
}

// Shell returns the embedded shell state for the core renderer.
func (p *PageData) Shell() *module.ShellData { return &p.ShellData }

// factionNames builds the faction ID → name map from the stored
// factions snapshot (the near-static GET /universe/factions/ list).
// Empty until the worker's first intel pass lands it.
func factionNames(ctx context.Context, deps module.Deps) map[int64]string {
	var factions esi.Factions
	if !deps.LoadGlobalSnapshot(ctx, esi.GlobalFactions, &factions) {
		return nil
	}
	names := make(map[int64]string, len(factions))
	for _, f := range factions {
		if f.Name != "" {
			names[f.FactionID] = f.Name
		}
	}
	return names
}

// factionDisplay renders a faction ID: the name when the factions
// snapshot has landed, a fallback otherwise.
func factionDisplay(names map[int64]string, id int64) string {
	if name, ok := names[id]; ok {
		return name
	}
	return fmt.Sprintf("Faction #%d", id)
}

// ---------------------------------------------------------------------------
// Wars.
// ---------------------------------------------------------------------------

// warRow is one line of the wars page. Rows whose detail payload
// has not landed yet carry Warming=true and placeholder fields.
type warRow struct {
	ID         int64
	Aggressor  template.HTML // linked organization name
	Defender   template.HTML
	Allies     []template.HTML // allied defenders, "" when none
	State      string          // Active | Retracted … | Ended …
	Declared   string
	Mutual     bool
	OpenAllies bool
	AggKills   string // "12 ships · 1,234,567.89 ISK destroyed"
	DefKills   string
	Yours      bool // aggressor/defender/ally is one of the user's corporations
	Warming    bool
}

// warsView is the Wars page body.
type warsView struct {
	Loaded  bool
	Warming bool // no war list snapshot yet
	Rows    []warRow
	Total   int // wars in the stored list (before the cap)
}

// handleWars renders the Wars page from the stored war ID list joined
// with the warmed war details.
func handleWars(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := PageData{ShellData: deps.PublicShell(ctx)} // public: no session needed
		data.HeadMeta = deps.HeadMeta(module.PageMeta{Title: "Wars", Description: "Current EVE Online wars: who is fighting, kills and ISK destroyed, from data refreshed from ESI.", CanonicalPath: "/intel/wars/"})

		view := &warsView{}
		data.IntelWars = view

		var list esi.WarList
		if !deps.LoadGlobalSnapshot(ctx, esi.GlobalWars, &list) {
			view.Warming = true
			deps.Render(ctx, w, http.StatusOK, "intel_wars.html", &data)
			return
		}
		view.Loaded = true
		view.Total = len(list)

		// Details warm for the newest wars first (the worker's
		// window), and a page rarely wants more than the freshest.
		ids := list
		if window := deps.WarDetailWindow(); len(ids) > window {
			ids = ids[:window]
		}

		yourCorps := deps.UserCorporationIDs(ctx, deps.UserID(ctx))
		for _, id := range ids {
			view.Rows = append(view.Rows, buildWarRow(ctx, deps, id, yourCorps))
		}

		deps.Render(ctx, w, http.StatusOK, "intel_wars.html", &data)
	}
}

// buildWarRow builds one wars-page line from the stored detail. A
// missing detail (the worker has not warmed it yet) yields the warming
// placeholder row.
func buildWarRow(ctx context.Context, deps module.Deps, warID int64, yourCorps map[int64]bool) warRow {
	row := warRow{ID: warID, Warming: true, Aggressor: "—", Defender: "—"}

	stored, err := deps.DB().GetWarDetail(ctx, warID)
	if err != nil {
		return row
	}
	var war esi.War
	if err := json.Unmarshal([]byte(stored.Payload), &war); err != nil {
		logging.Errorf("intel: decode war detail %d: %v", warID, err)
		return row
	}
	row.Warming = false

	row.Aggressor = deps.OrgLink(ctx, war.Aggressor.CorporationID, war.Aggressor.AllianceID)
	row.Defender = deps.OrgLink(ctx, war.Defender.CorporationID, war.Defender.AllianceID)
	row.AggKills = warPartyRecord(war.Aggressor)
	row.DefKills = warPartyRecord(war.Defender)
	row.Declared = deps.FormatFinish(war.Declared)
	row.Mutual = war.Mutual
	row.OpenAllies = war.OpenForAllies
	switch {
	case war.Finished != "":
		row.State = "Ended " + deps.FormatFinish(war.Finished)
	case war.Retracted != "":
		row.State = "Retracted " + deps.FormatFinish(war.Retracted)
	case war.Started != "":
		row.State = "Active"
	default:
		row.State = "Pending start" // declared; the 24h warm-up is still running
	}

	var allies []template.HTML
	for _, ally := range war.Allies {
		allies = append(allies, deps.OrgLink(ctx, ally.CorporationID, ally.AllianceID))
	}
	row.Allies = allies

	row.Yours = warInvolves(war, yourCorps)
	return row
}

// warPartyRecord formats a war side's kill record.
func warPartyRecord(p esi.WarParty) string {
	return fmt.Sprintf("%s ships · %s ISK destroyed",
		esi.FormatInt(p.ShipsKilled), esi.FormatISK(p.ISKDestroyed))
}

// warInvolves reports whether any party of the war (aggressor,
// defender, or a defending ally) is one of the user's corporations.
// Alliance parties can't match: the map only knows corporation
// membership.
func warInvolves(war esi.War, yourCorps map[int64]bool) bool {
	if len(yourCorps) == 0 {
		return false
	}
	if yourCorps[war.Aggressor.CorporationID] || yourCorps[war.Defender.CorporationID] {
		return true
	}
	for _, ally := range war.Allies {
		if yourCorps[ally.CorporationID] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Incursions.
// ---------------------------------------------------------------------------

// incursionRow is one line of the incursions page.
type incursionRow struct {
	Constellation string
	Staging       module.PlaceRef // staging solar system (linked when the SDE knows it)
	// The staging system's security pill (the fleet overview's
	// badge): staging only — the affected systems carry none.
	StagingSecKnown bool
	StagingSecText  string
	StagingSecTier  int
	State           string // humanized
	InfluencePct    int    // 0..100, drives the bar
	Boss            bool   // final-encounter boss present
	FactionName     string
	Systems         []module.PlaceRef // affected (infested) systems
}

// incursionsView is the Incursions page body.
type incursionsView struct {
	Loaded  bool
	Warming bool // no incursions snapshot yet
	Rows    []incursionRow
}

// handleIncursions renders the Incursions page from the stored
// snapshot: constellation names from the worker-warmed cache, system
// names from the SDE tables, faction names from the factions snapshot.
func handleIncursions(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := PageData{ShellData: deps.PublicShell(ctx)} // public: no session needed
		data.HeadMeta = deps.HeadMeta(module.PageMeta{Title: "Incursions", Description: "Active EVE Online incursions by constellation, with staging systems and status, from data refreshed from ESI.", CanonicalPath: "/intel/incursions/"})

		view := &incursionsView{}
		data.IntelIncursions = view

		var incursions esi.Incursions
		if !deps.LoadGlobalSnapshot(ctx, esi.GlobalIncursions, &incursions) {
			view.Warming = true
			deps.Render(ctx, w, http.StatusOK, "intel_incursions.html", &data)
			return
		}
		view.Loaded = true

		factions := factionNames(ctx, deps)
		for _, inc := range incursions {
			row := incursionRow{
				Constellation: fmt.Sprintf("Constellation #%d", inc.ConstellationID),
				Staging:       deps.SystemRef(ctx, inc.StagingSystemID),
				State:         humanizeState(deps, inc.State),
				Boss:          inc.HasBoss,
				FactionName:   factionDisplay(factions, inc.FactionID),
			}
			if name, ok := deps.ConstellationName(inc.ConstellationID); ok {
				row.Constellation = name
			}
			row.StagingSecText, row.StagingSecTier, row.StagingSecKnown = deps.SystemSecurity(ctx, inc.StagingSystemID)
			pct := int(inc.Influence*100 + 0.5)
			if pct < 0 {
				pct = 0
			}
			if pct > 100 {
				pct = 100
			}
			row.InfluencePct = pct
			for _, systemID := range inc.InfestedSystems {
				row.Systems = append(row.Systems, deps.SystemRef(ctx, systemID))
			}
			view.Rows = append(view.Rows, row)
		}

		deps.Render(ctx, w, http.StatusOK, "intel_incursions.html", &data)
	}
}

// humanizeState renders an ESI state enum ("established",
// "armor_reinforce") as a capitalized phrase ("Established",
// "Armor reinforce").
func humanizeState(deps module.Deps, s string) string {
	s = deps.HumanizeEnum(s)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// Faction warfare.
// ---------------------------------------------------------------------------

// maxFWSystemsShown caps the contested-systems list (highest contested
// first); the full frontier is longer than a page wants.
const maxFWSystemsShown = 50

// fwFactionCard is one faction's summary card on the FW page, joined
// from /fw/stats/ and the factions snapshot names.
type fwFactionCard struct {
	Name             string
	Systems          string // systems controlled
	Pilots           string
	KillsYesterday   string
	KillsLastWeek    string
	VictoryYesterday string
	VictoryLastWeek  string
}

// fwSystemRow is one contested-system line.
type fwSystemRow struct {
	System   module.PlaceRef
	Region   string // the system's region name, "" when the SDE has none
	Occupier string
	Owner    string
	State    string // humanized contested label
	Percent  int    // contested %, -1 when the state has no live contest
	// The system's security pill (the fleet overview's badge),
	// resolved after the row cap so lookups stay bounded by the
	// rows actually rendered.
	SecKnown bool
	SecText  string
	SecTier  int
}

// fwView is the Faction Warfare page body.
type fwView struct {
	Loaded   bool
	Warming  bool // neither stats nor systems snapshot yet
	Factions []fwFactionCard
	Systems  []fwSystemRow
	Total    int // systems in the stored list (before the cap)
}

// handleFW renders the Faction Warfare page: faction summary cards from
// /fw/stats/ and the contested-systems list from /fw/systems/, both
// from the global store.
func handleFW(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := PageData{ShellData: deps.PublicShell(ctx)} // public: no session needed
		data.HeadMeta = deps.HeadMeta(module.PageMeta{Title: "Faction Warfare", Description: "Faction warfare in EVE Online: militia strength, frontlines and contested systems, from data refreshed from ESI.", CanonicalPath: "/intel/fw/"})

		view := &fwView{}
		data.IntelFW = view

		var stats esi.FWStats
		var systems esi.FWSystems
		haveStats := deps.LoadGlobalSnapshot(ctx, esi.GlobalFWStats, &stats)
		haveSystems := deps.LoadGlobalSnapshot(ctx, esi.GlobalFWSystems, &systems)
		if !haveStats && !haveSystems {
			view.Warming = true
			deps.Render(ctx, w, http.StatusOK, "intel_fw.html", &data)
			return
		}
		view.Loaded = true

		factions := factionNames(ctx, deps)

		if haveStats {
			sorted := append(esi.FWStats(nil), stats...)
			sort.SliceStable(sorted, func(i, j int) bool {
				return sorted[i].SystemsControlled > sorted[j].SystemsControlled
			})
			for _, s := range sorted {
				view.Factions = append(view.Factions, fwFactionCard{
					Name:             factionDisplay(factions, s.FactionID),
					Systems:          esi.FormatInt(s.SystemsControlled),
					Pilots:           esi.FormatInt(s.Pilots),
					KillsYesterday:   esi.FormatInt(s.Kills.Yesterday),
					KillsLastWeek:    esi.FormatInt(s.Kills.LastWeek),
					VictoryYesterday: esi.FormatInt(s.VictoryPoints.Yesterday),
					VictoryLastWeek:  esi.FormatInt(s.VictoryPoints.LastWeek),
				})
			}
		}

		if haveSystems {
			view.Total = len(systems)
			rows := make([]fwSystemRow, 0, len(systems))
			for _, sys := range systems {
				rows = append(rows, fwSystemRow{
					System:   deps.SystemRef(ctx, sys.SolarSystemID),
					Occupier: factionDisplay(factions, sys.OccupierFactionID),
					Owner:    factionDisplay(factions, sys.OwnerFactionID),
					State:    humanizeState(deps, sys.Contested),
					Percent:  contestedPercent(sys),
				})
			}
			// Highest contested first; the state-only rows (no live
			// contest) sink to the bottom.
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Percent != rows[j].Percent {
					return rows[i].Percent > rows[j].Percent
				}
				return rows[i].System.Name < rows[j].System.Name
			})
			if len(rows) > maxFWSystemsShown {
				rows = rows[:maxFWSystemsShown]
			}
			for i := range rows {
				rows[i].SecText, rows[i].SecTier, rows[i].SecKnown = deps.SystemSecurity(ctx, rows[i].System.SystemID)
				rows[i].Region, _ = deps.SystemRegion(ctx, rows[i].System.SystemID)
			}
			// The table reads region by region (system name within
			// a region); rows whose region the SDE cannot name sink
			// below the named ones.
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Region != rows[j].Region {
					if rows[i].Region == "" {
						return false
					}
					if rows[j].Region == "" {
						return true
					}
					return rows[i].Region < rows[j].Region
				}
				return rows[i].System.Name < rows[j].System.Name
			})
			view.Systems = rows
		}

		deps.Render(ctx, w, http.StatusOK, "intel_fw.html", &data)
	}
}

// contestedPercent computes a front-line system's contested
// percentage: victory points accrued over the flip threshold, the
// ratio the in-game display tracks. States without a live contest
// (uncontested/captured) report -1 so the page can show a dash and
// sorting sinks them.
func contestedPercent(sys esi.FWSystem) int {
	if sys.Contested != "contested" && sys.Contested != "vulnerable" {
		return -1
	}
	if sys.VictoryPointsThreshold <= 0 {
		return -1
	}
	pct := int(sys.VictoryPoints * 100 / sys.VictoryPointsThreshold)
	if pct > 100 {
		pct = 100
	}
	return pct
}
