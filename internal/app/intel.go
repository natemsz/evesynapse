package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Intel cluster: wars, incursions and faction warfare, plus the
// Tranquility status line on Home.
//
// All of it is public ESI data, so it lives in the global store
// (global_snapshots + war_details) that the worker keeps warm, not the
// per-character snapshot table. Handlers read the store and the local
// name caches only.
// ---------------------------------------------------------------------------

// globalKindOrder fixes the global snapshot kinds' display order
// on the Sync page (ListGlobalSnapshots orders alphabetically).
var globalKindOrder = []string{
	esi.GlobalStatus, esi.GlobalWars, esi.GlobalIncursions,
	esi.GlobalFWSystems, esi.GlobalFWStats, esi.GlobalFactions,
}

// factionNames builds the faction ID → name map from the stored
// factions snapshot (the near-static GET /universe/factions/
// list). Empty until the worker's first intel pass lands it.
func (app *Application) factionNames(ctx context.Context) map[int64]string {
	var factions esi.Factions
	if !app.loadGlobalSnapshot(ctx, esi.GlobalFactions, &factions) {
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

// userCorporationIDs returns every corporation the given user's
// characters belong to, from the worker-maintained
// character → corporation map. The wars page uses it to flag
// wars that involve the user's corporations. The query is
// scoped to the signed-in user: another user's corporations
// are never flagged as yours. Map errors degrade to "no known
// corporations" (nothing flagged).
func (app *Application) userCorporationIDs(ctx context.Context, userID int64) map[int64]bool {
	ids, err := app.queries.ListCorporationIDsByUser(ctx, userID)
	if err != nil {
		logging.Errorf("intel: list user corporations: %v", err)
		return nil
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// orgDisplay renders a war party as a link to its public page:
// alliance names to the alliance page, corporation names to the
// corporation page, "#<id>" labels while a name is still
// warming. Never networks.
func (app *Application) orgDisplay(ctx context.Context, corporationID, allianceID int64) template.HTML {
	if allianceID > 0 {
		return allianceLink(allianceID, app.allianceDisplayName(ctx, allianceID))
	}
	if corporationID > 0 {
		return corpLink(corporationID, app.corpDisplayName(ctx, corporationID))
	}
	return "—"
}

// ---------------------------------------------------------------------------
// Wars.
// ---------------------------------------------------------------------------

// maxWarsShown caps the wars list: details warm for the newest
// wars first, and a page rarely wants more than the freshest.
const maxWarsShown = 50

// warRow is one line of the wars page. Rows whose detail payload
// has not landed yet carry Warming=true and placeholder fields.
type warRow struct {
	ID         int64
	Aggressor  template.HTML // linked organization name (see orgDisplay)
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

// handleIntelWars renders the Wars page from the stored war ID
// list joined with the warmed war details.
func (app *Application) handleIntelWars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	view := &warsView{}
	data.IntelWars = view

	var list esi.WarList
	if !app.loadGlobalSnapshot(ctx, esi.GlobalWars, &list) {
		view.Warming = true
		app.render(ctx, w, http.StatusOK, "intel_wars.html", data)
		return
	}
	view.Loaded = true
	view.Total = len(list)

	ids := list
	if len(ids) > maxWarsShown {
		ids = ids[:maxWarsShown]
	}

	yourCorps := app.userCorporationIDs(ctx, app.userID(ctx))
	for _, id := range ids {
		view.Rows = append(view.Rows, app.warRow(ctx, id, yourCorps))
	}

	app.render(ctx, w, http.StatusOK, "intel_wars.html", data)
}

// warRow builds one wars-page line from the stored detail. A
// missing detail (worker has not warmed it yet) yields the
// warming placeholder row.
func (app *Application) warRow(ctx context.Context, warID int64, yourCorps map[int64]bool) warRow {
	row := warRow{ID: warID, Warming: true, Aggressor: "—", Defender: "—"}

	stored, err := app.queries.GetWarDetail(ctx, warID)
	if err != nil {
		return row
	}
	var war esi.War
	if err := json.Unmarshal([]byte(stored.Payload), &war); err != nil {
		logging.Errorf("intel: decode war detail %d: %v", warID, err)
		return row
	}
	row.Warming = false

	row.Aggressor = app.orgDisplay(ctx, war.Aggressor.CorporationID, war.Aggressor.AllianceID)
	row.Defender = app.orgDisplay(ctx, war.Defender.CorporationID, war.Defender.AllianceID)
	row.AggKills = warPartyRecord(war.Aggressor)
	row.DefKills = warPartyRecord(war.Defender)
	row.Declared = formatFinish(war.Declared)
	row.Mutual = war.Mutual
	row.OpenAllies = war.OpenForAllies
	switch {
	case war.Finished != "":
		row.State = "Ended " + formatFinish(war.Finished)
	case war.Retracted != "":
		row.State = "Retracted " + formatFinish(war.Retracted)
	case war.Started != "":
		row.State = "Active"
	default:
		row.State = "Pending start" // declared; the 24h warm-up is still running
	}

	var allies []template.HTML
	for _, ally := range war.Allies {
		allies = append(allies, app.orgDisplay(ctx, ally.CorporationID, ally.AllianceID))
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
// defender, or a defending ally) is one of the user's
// corporations. Alliance parties can't match: the map only knows
// corporation membership.
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
	Staging       placeRef // staging solar system (linked when the SDE knows it)
	State         string   // humanized
	InfluencePct  int      // 0..100, drives the bar
	Boss          bool     // final-encounter boss present
	FactionName   string
	Systems       []placeRef // affected (infested) systems
}

// incursionsView is the Incursions page body.
type incursionsView struct {
	Loaded  bool
	Warming bool // no incursions snapshot yet
	Rows    []incursionRow
}

// handleIntelIncursions renders the Incursions page from the
// stored snapshot: constellation names from the worker-warmed
// cache, system names from the SDE tables, faction names from
// the factions snapshot.
func (app *Application) handleIntelIncursions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	view := &incursionsView{}
	data.IntelIncursions = view

	var incursions esi.Incursions
	if !app.loadGlobalSnapshot(ctx, esi.GlobalIncursions, &incursions) {
		view.Warming = true
		app.render(ctx, w, http.StatusOK, "intel_incursions.html", data)
		return
	}
	view.Loaded = true

	factions := app.factionNames(ctx)
	for _, inc := range incursions {
		row := incursionRow{
			Constellation: fmt.Sprintf("Constellation #%d", inc.ConstellationID),
			Staging:       app.linkPlace(ctx, inc.StagingSystemID, app.locationTitle(ctx, inc.StagingSystemID, "solar_system")),
			State:         humanizeState(inc.State),
			Boss:          inc.HasBoss,
			FactionName:   factionDisplay(factions, inc.FactionID),
		}
		if name, ok := app.esi.CachedConstellationName(inc.ConstellationID); ok {
			row.Constellation = name
		}
		pct := int(inc.Influence*100 + 0.5)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		row.InfluencePct = pct
		for _, systemID := range inc.InfestedSystems {
			row.Systems = append(row.Systems, app.linkPlace(ctx, systemID, app.locationTitle(ctx, systemID, "solar_system")))
		}
		view.Rows = append(view.Rows, row)
	}

	app.render(ctx, w, http.StatusOK, "intel_incursions.html", data)
}

// humanizeState renders an ESI state enum ("established",
// "armor_reinforce") as a capitalized phrase ("Established",
// "Armor reinforce").
func humanizeState(s string) string {
	s = humanizeEnum(s)
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// Faction warfare.
// ---------------------------------------------------------------------------

// maxFWSystemsShown caps the contested-systems list (highest
// contested first); the full frontier is longer than a page
// wants.
const maxFWSystemsShown = 50

// fwFactionCard is one faction's summary card on the FW page,
// joined from /fw/stats/ and the factions snapshot names.
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
	System   placeRef
	Occupier string
	Owner    string
	State    string // humanized contested label
	Percent  int    // contested %, -1 when the state has no live contest
}

// fwView is the Faction Warfare page body.
type fwView struct {
	Loaded   bool
	Warming  bool // neither stats nor systems snapshot yet
	Factions []fwFactionCard
	Systems  []fwSystemRow
	Total    int // systems in the stored list (before the cap)
}

// handleIntelFW renders the Faction Warfare page: four-faction
// summary cards from /fw/stats/ and the contested-systems list
// from /fw/systems/, both from the global store.
func (app *Application) handleIntelFW(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	view := &fwView{}
	data.IntelFW = view

	var stats esi.FWStats
	var systems esi.FWSystems
	haveStats := app.loadGlobalSnapshot(ctx, esi.GlobalFWStats, &stats)
	haveSystems := app.loadGlobalSnapshot(ctx, esi.GlobalFWSystems, &systems)
	if !haveStats && !haveSystems {
		view.Warming = true
		app.render(ctx, w, http.StatusOK, "intel_fw.html", data)
		return
	}
	view.Loaded = true

	factions := app.factionNames(ctx)

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
				System:   app.linkPlace(ctx, sys.SolarSystemID, app.locationTitle(ctx, sys.SolarSystemID, "solar_system")),
				Occupier: factionDisplay(factions, sys.OccupierFactionID),
				Owner:    factionDisplay(factions, sys.OwnerFactionID),
				State:    humanizeState(sys.Contested),
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
		view.Systems = rows
	}

	app.render(ctx, w, http.StatusOK, "intel_fw.html", data)
}

// contestedPercent computes a front-line system's contested
// percentage: victory points accrued over the flip threshold,
// the ratio the in-game display tracks. States without a live
// contest (uncontested/captured) report -1 so the page can show
// a dash and sorting sinks them.
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

// ---------------------------------------------------------------------------
// Server status (Home page line).
// ---------------------------------------------------------------------------

// serverStatusView is the Home page's Tranquility line. Nil on
// pageData until the worker's first intel pass lands the status
// snapshot — Home renders exactly as before without it.
type serverStatusView struct {
	Players string // formatted
}

// loadServerStatus reads the stored Tranquility status for the
// Home page line. DB only; false until the snapshot exists.
func (app *Application) loadServerStatus(ctx context.Context) (*serverStatusView, bool) {
	var status esi.ServerStatus
	if !app.loadGlobalSnapshot(ctx, esi.GlobalStatus, &status) {
		return nil, false
	}
	return &serverStatusView{Players: esi.FormatInt(status.Players)}, true
}
