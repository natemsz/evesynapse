package app

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"html/template"
	"sort"
	"strings"
	"time"

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
	esi.GlobalSystemKills, esi.GlobalSystemJumps, esi.GlobalSovMap,
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

// maxWarsShown is how many of the newest wars the worker warms details
// for; the Intel module's wars page lists the same window (through
// Deps.WarDetailWindow).
const maxWarsShown = 50

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
// Server status (Home page line).
// ---------------------------------------------------------------------------

// serverStatusView is the Home page's Tranquility line. Nil on
// pageData until the first status refresh lands the status
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

// esiDownAfter is how stale the stored Tranquility status may get
// before the Overview bar calls ESI down. The status refresh
// renews it about every minute on a 30-second ESI cache window,
// so ten minutes without a fresh copy means ESI has stopped
// answering (daily downtime, an outage), not a slow refresh.
const esiDownAfter = 10 * time.Minute

// esiHealthView is the Overview bar's status pill: the combined
// ESI and our-side health and, while ESI is up, Tranquility's
// player count.
type esiHealthView struct {
	State   string // "ok", "limited", "degraded", "down", "ours-lagging", "ours-down", or "pending" (nothing stored yet)
	Label   string // the pill's words
	Players string // formatted player count; empty while down or unknown
	Title   string // the hover text: what the state means
}

// esiHealth reads the pill's state from what is already held: the
// stored status snapshot's age (a fetch that keeps failing leaves
// it to age), the in-memory ESI error budget, and this server's
// own pipeline states (ourSideState). DB only, no ESI.
//
// One combined state, blame attributed correctly: ESI down first
// (our lag then is ESI's fault), then lag that traces to ESI
// (CCP reports route problems, or our own calls are failing at
// the degraded rate: the headline is ESI degraded, not our
// sync), then ours-down, then ours-lagging, then ESI limited,
// then the route problems CCP itself reports, then healthy. The
// player count shows whenever ESI is answering, our-side states
// included.
func (app *Application) esiHealth(ctx context.Context) *esiHealthView {
	snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalStatus)
	if err != nil {
		return &esiHealthView{State: "pending", Label: "ESI", Title: "Checking ESI. The first status check has not landed yet."}
	}
	if time.Since(snap.FetchedAt) > esiDownAfter {
		return &esiHealthView{State: "down", Label: "ESI down",
			Title: "ESI has not answered since " + snap.FetchedAt.UTC().Format("15:04") + " UTC. Figures here are the last ones stored."}
	}
	now := time.Now()
	view := &esiHealthView{State: "ok", Label: "ESI healthy", Title: "ESI is answering normally."}
	var status esi.ServerStatus
	vip := false
	if json.Unmarshal([]byte(snap.Payload), &status) == nil {
		view.Players = esi.FormatInt(status.Players)
		vip = status.VIP
	}
	limited := app.esi != nil && app.esi.ErrorBudgetLow()
	if state, problem := app.ourSideState(ctx, now); state != "" {
		// A lag with ESI trouble behind it is ESI's, not ours:
		// either witness (CCP's route report, or our own calls
		// failing at the degraded rate) leads the headline.
		if areas := app.degradedRouteAreas(ctx, now); len(areas) > 0 {
			view.State, view.Label = "degraded", "ESI degraded"
			view.Title = problem + " CCP reports problems with " + strings.Join(areas, ", ") + ", which is why sync is behind."
			return view
		}
		if app.esiErrorShareHigh(ctx, now) {
			view.State, view.Label = "degraded", "ESI degraded"
			view.Title = problem + " ESI calls are failing, which is why sync is behind."
			return view
		}
		view.State = state
		view.Label = "Sync behind"
		if state == "ours-down" {
			view.Label = "Sync stalled"
		}
		view.Title = problem
		if limited {
			view.Title += " ESI is answering, but this server is close to its error limit, so syncing is paused briefly."
		} else {
			view.Title += " ESI itself is answering normally."
		}
		return view
	}
	if limited {
		view.State, view.Label = "limited", "ESI limited"
		view.Title = "ESI is answering, but this server is close to its error limit, so syncing is paused briefly."
		return view
	}
	if areas := app.degradedRouteAreas(ctx, now); len(areas) > 0 {
		view.State, view.Label = "degraded", "ESI degraded"
		view.Title = "ESI is answering, but CCP reports problems with " + strings.Join(areas, ", ") + "."
		return view
	}
	if vip {
		view.Title = "ESI is answering; Tranquility is in VIP mode (developers only)."
	}
	return view
}

// statusRefreshInterval is how often the standalone status
// refresh asks ESI for Tranquility's status: about once a minute,
// on the snapshot's own schedule.
const statusRefreshInterval = time.Minute

// refreshServerStatus fetches Tranquility's status through the
// ESI client and stores it as the GlobalStatus snapshot the
// readers above use, then fetches CCP's per-route health the
// same way. It is both snapshots' one writer. A failed fetch
// stores nothing: the snapshot is left to age, which is the
// readers' down signal, and the last known route health stands.
func (app *Application) refreshServerStatus(ctx context.Context) {
	if app.esi == nil {
		return
	}
	if err := app.esi.FetchAndStoreGlobalSnapshot(ctx, esi.GlobalStatus); err != nil {
		if ctx.Err() == nil {
			logging.Warnf("status refresh: fetch Tranquility status: %v", err)
		}
	}
	if err := app.esi.FetchAndStoreRouteStatus(ctx); err != nil {
		if ctx.Err() == nil {
			logging.Warnf("status refresh: fetch route status: %v", err)
		}
	}
}

// runServerStatusRefresh keeps the stored Tranquility status
// fresh on its own ticker beside the worker cycle (started from
// runWorker), so the status surfaces recover as soon as ESI does
// instead of waiting for a whole cycle to complete. It attempts
// every tick even while the worker is backed off on the error
// budget; detecting recovery is its whole job.
func (app *Application) runServerStatusRefresh(ctx context.Context) {
	app.refreshServerStatus(ctx)
	ticker := time.NewTicker(statusRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			app.refreshServerStatus(ctx)
		}
	}
}

// degradedRouteAreas reads the stored route health: the areas
// with a route CCP marks degraded or recovering, named in plain
// words and sorted. The stored copy speaks only while it is
// inside the same fuse as the Tranquility snapshot; a stale or
// absent copy, like an all-healthy one, names nothing.
func (app *Application) degradedRouteAreas(ctx context.Context, now time.Time) []string {
	snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalRouteStatus)
	if err != nil || now.Sub(snap.FetchedAt) > esiDownAfter {
		return nil
	}
	var meta esi.MetaStatus
	if json.Unmarshal([]byte(snap.Payload), &meta) != nil {
		return nil
	}
	seen := map[string]bool{}
	var areas []string
	for _, r := range meta.Routes {
		if area := routeAreaName(r.Path); area != "" && !seen[area] {
			seen[area] = true
			areas = append(areas, area)
		}
	}
	sort.Strings(areas)
	return areas
}

// routeAreaName names the area a route belongs to in ordinary
// words: its first path segment, except that universe routes
// name the thing under universe (structures, systems, types),
// because "universe" alone says nothing.
func routeAreaName(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	if parts[0] == "universe" && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

// ourSideState evaluates this server's own pipelines for the pill:
// the market sweeps and the worker, read by the same computations
// and thresholds the admin status page uses (statusMarket and
// statusWorker in core_status.go). No new probes, no new
// thresholds. It returns the pill's our-side state ("ours-down"
// when the market is down or the worker is stalled, "ours-lagging"
// when the market is lagging or the worker is lagging, "" when
// both are fine) and a sentence naming what is behind and by how
// much.
func (app *Application) ourSideState(ctx context.Context, now time.Time) (string, string) {
	market, marketSince := app.statusMarket(ctx, now, true)
	worker, workerSince := app.statusWorker(now)
	state := ""
	switch {
	case market == statusDown || worker == statusDown:
		state = "ours-down"
	case market == statusDegraded || worker == statusDegraded:
		state = "ours-lagging"
	default:
		return "", ""
	}
	var parts []string
	if market != statusOK {
		if marketSince.IsZero() {
			parts = append(parts, "Market orders have not been swept yet.")
		} else {
			parts = append(parts, "Market orders are "+pillAge(now.Sub(marketSince))+" old.")
		}
	}
	if worker != statusOK {
		from := workerSince
		if from.IsZero() {
			from = app.startedAt
		}
		parts = append(parts, "The worker has not started a cycle for "+pillAge(now.Sub(from))+".")
	}
	return state, strings.Join(parts, " ")
}

// pillAge writes an age the way the pill's hover text uses it:
// "5h12m", "42m", "30s".
func pillAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
