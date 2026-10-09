package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Solar system & station pages (/system/, /station/): the
// destinations every resolved system and station name now links
// to. Unlike the organization pages there is nothing to warm —
// both render entirely from the local SDE (sde_systems,
// sde_stations, sde_regions) the importer already maintains, so a
// render is a couple of Postgres reads and never calls out. An id
// the SDE doesn't know (or a system whose data hasn't imported
// yet) settles in an honest not-in-the-star-map state rather than
// an error. Region names render as text: there is no region page.
// ---------------------------------------------------------------------------

// systemStationLine is one NPC station listed on a system page.
type systemStationLine struct {
	StationID int64
	Name      string
}

// systemPageView is the /system/ page body.
type systemPageView struct {
	SystemID int64
	State    string // "ready" | "missing"
	Name     string
	Security string // EVE-style one-decimal security, e.g. "0.9"
	RegionID int64
	Region   string
	Stations []systemStationLine
}

// stationPageView is the /station/ page body.
type stationPageView struct {
	StationID int64
	State     string // "ready" | "missing"
	Name      string
	SystemID  int64
	System    string
	Region    string
}

// handleSystemPage renders one solar system's SDE record.
func (app *Application) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.System = app.loadSystemView(ctx, id)
	app.render(ctx, w, http.StatusOK, "system.html", data)
}

// handleStationPage renders one NPC station's SDE record.
func (app *Application) handleStationPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	id, err := strconv.ParseInt(r.URL.Query().Get("station"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.Station = app.loadStationView(ctx, id)
	app.render(ctx, w, http.StatusOK, "station.html", data)
}

// loadSystemView builds the render model for a system page from
// the SDE tables. Cache-only; shared shape with loadStationView.
func (app *Application) loadSystemView(ctx context.Context, id int64) *systemPageView {
	view := &systemPageView{SystemID: id, State: "missing"}

	sys, err := app.queries.GetSDESystem(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("system page: read system %d: %v", id, err)
		}
		return view
	}

	view.State = "ready"
	view.Name = sys.Name
	view.Security = fmt.Sprintf("%.1f", sys.Security)
	view.RegionID = sys.RegionID
	if region, rerr := app.queries.GetSDERegion(ctx, sys.RegionID); rerr == nil {
		view.Region = region.Name
	}

	stations, serr := app.queries.ListSDEStationsBySystem(ctx, id)
	if serr != nil {
		logging.Errorf("system page: list stations for system %d: %v", id, serr)
		return view
	}
	for _, st := range stations {
		view.Stations = append(view.Stations, systemStationLine{StationID: st.StationID, Name: st.Name})
	}
	return view
}

// loadStationView builds the render model for a station page from
// the SDE tables: the station, its system (linked), and the
// region name as text. Cache-only.
func (app *Application) loadStationView(ctx context.Context, id int64) *stationPageView {
	view := &stationPageView{StationID: id, State: "missing"}

	st, err := app.queries.GetSDEStation(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("station page: read station %d: %v", id, err)
		}
		return view
	}

	view.State = "ready"
	view.Name = st.Name
	view.SystemID = st.SystemID
	if sys, serr := app.queries.GetSDESystem(ctx, st.SystemID); serr == nil {
		view.System = sys.Name
		if region, rerr := app.queries.GetSDERegion(ctx, sys.RegionID); rerr == nil {
			view.Region = region.Name
		}
	}
	return view
}

// linkPlace classifies a rendered location title for the name
// policy: an id the SDE knows as an NPC station links to the
// station page, one it knows as a solar system links to the
// system page, and a player structure links to the structure page
// once the app can render something real for it (a resolved name
// or stored context) — anything else, containers and ids nobody
// has resolved yet, stays plain text, so a link never lands on a
// page that can't render. The title itself is whatever title the
// caller already resolved (its fallbacks are unchanged).
// Cache-only.
func (app *Application) linkPlace(ctx context.Context, locationID int64, title string) placeRef {
	ref := placeRef{Name: title}
	if locationID <= 0 {
		return ref
	}
	if isStructureID(locationID) {
		if app.structureLinkable(ctx, locationID) {
			ref.StructureID = locationID
		}
		return ref
	}
	if locationID > 1_000_000_000 {
		return ref
	}
	if row, err := app.queries.GetSDEStation(ctx, locationID); err == nil && row.Name != "" {
		ref.StationID = locationID
		return ref
	}
	if row, err := app.queries.GetSDESystem(ctx, locationID); err == nil && row.Name != "" {
		ref.SystemID = locationID
	}
	return ref
}

// linkPlaceMemo is linkPlace with a per-render memo: row-building
// loops that touch the same locations repeatedly (corp orders,
// assets, killmails) resolve each location once instead of
// re-querying the SDE tables per row.
func (app *Application) linkPlaceMemo(ctx context.Context, memo map[int64]placeRef, locationID int64, title string) placeRef {
	if ref, ok := memo[locationID]; ok {
		return ref
	}
	ref := app.linkPlace(ctx, locationID, title)
	memo[locationID] = ref
	return ref
}
