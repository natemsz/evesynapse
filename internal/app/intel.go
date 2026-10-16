package app

import (
	"context"
	"encoding/json"
	"html/template"
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

// esiDownAfter is how stale the stored Tranquility status may get
// before the Overview bar calls ESI down. The worker refreshes it
// every cycle (about a minute) on a 30-second ESI cache window, so
// ten minutes without a fresh copy means ESI has stopped answering
// (daily downtime, an outage), not a slow cycle.
const esiDownAfter = 10 * time.Minute

// esiHealthView is the Overview bar's status pill: ESI's health and,
// while it is up, Tranquility's player count.
type esiHealthView struct {
	State   string // "ok", "limited", "down" or "pending" (nothing stored yet)
	Label   string // the pill's words
	Players string // formatted player count; empty while down or unknown
	Title   string // the hover text: what the state means
}

// esiHealth reads the pill's state from what is already held: the
// stored status snapshot's age (a fetch that keeps failing leaves
// it to age) and the in-memory ESI error budget. DB only, no ESI.
func (app *Application) esiHealth(ctx context.Context) *esiHealthView {
	snap, err := app.queries.GetGlobalSnapshot(ctx, esi.GlobalStatus)
	if err != nil {
		return &esiHealthView{State: "pending", Label: "ESI", Title: "Checking ESI — the first status check has not landed yet."}
	}
	if time.Since(snap.FetchedAt) > esiDownAfter {
		return &esiHealthView{State: "down", Label: "ESI down",
			Title: "ESI has not answered since " + snap.FetchedAt.UTC().Format("15:04") + " UTC. Figures here are the last ones stored."}
	}
	view := &esiHealthView{State: "ok", Label: "ESI healthy", Title: "ESI is answering normally."}
	var status esi.ServerStatus
	if json.Unmarshal([]byte(snap.Payload), &status) == nil {
		view.Players = esi.FormatInt(status.Players)
		if status.VIP {
			view.Title = "ESI is answering; Tranquility is in VIP mode (developers only)."
		}
	}
	if app.esi != nil && app.esi.ErrorBudgetLow() {
		view.State, view.Label = "limited", "ESI limited"
		view.Title = "ESI is answering, but this server is close to its error limit, so syncing is paused briefly."
	}
	return view
}
