package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Solar system, region and station pages (/system/, /region/,
// /station/): where every resolved place name links to. All render
// from stored rows only -- the local SDE (sde_systems,
// sde_stations, sde_regions) plus the distilled activity and
// sovereignty tables the place collector keeps (schema 075) -- so
// there is nothing to warm. An id the SDE does not know settles in
// a not-in-the-star-map state.
// ---------------------------------------------------------------------------

// systemStationLine is one NPC station listed on a system page.
type systemStationLine struct {
	StationID int64
	Name      string
}

// placeActivityView is one place's distilled activity figures,
// display-ready: counts comma-grouped, daily averages at one
// decimal, and the average column's label, which names the window
// when the collection behind the figures is under a week.
type placeActivityView struct {
	ShipKills1h, PodKills1h, NPCKills1h, Jumps1h     string
	ShipKills24h, PodKills24h, NPCKills24h, Jumps24h string
	ShipKillsDay, PodKillsDay, NPCKillsDay, JumpsDay string
	AvgLabel                                         string
}

func placeActivityViewOf(ship1, pod1, npc1, jumps1, ship24, pod24, npc24, jumps24 int64, shipDay, podDay, npcDay, jumpsDay float64, hoursCovered int32) *placeActivityView {
	return &placeActivityView{
		ShipKills1h: esi.FormatInt(ship1), PodKills1h: esi.FormatInt(pod1),
		NPCKills1h: esi.FormatInt(npc1), Jumps1h: esi.FormatInt(jumps1),
		ShipKills24h: esi.FormatInt(ship24), PodKills24h: esi.FormatInt(pod24),
		NPCKills24h: esi.FormatInt(npc24), Jumps24h: esi.FormatInt(jumps24),
		ShipKillsDay: formatDayAvg(shipDay), PodKillsDay: formatDayAvg(podDay),
		NPCKillsDay: formatDayAvg(npcDay), JumpsDay: formatDayAvg(jumpsDay),
		AvgLabel: avgWindowLabel(hoursCovered),
	}
}

// avgWindowLabel names the daily-average column for the hours of
// collection behind the figures: a full week needs no qualifier.
func avgWindowLabel(hours int32) string {
	switch {
	case hours >= 7*24:
		return "Daily average"
	case hours >= 48:
		return fmt.Sprintf("Daily average (last %d days)", hours/24)
	case hours >= 24:
		return "Daily average (last day)"
	case hours == 1:
		return "Daily average (last hour)"
	default:
		return fmt.Sprintf("Daily average (last %d hours)", hours)
	}
}

// formatDayAvg renders a daily average at one decimal with the
// whole part comma-grouped, like the counts beside it.
func formatDayAvg(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	whole, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(strings.TrimPrefix(whole, "-"), 10, 64)
	if err != nil {
		return s
	}
	out := esi.FormatInt(n) + "." + frac
	if strings.HasPrefix(whole, "-") {
		return "-" + out
	}
	return out
}

// systemPageView is the /system/ page body.
type systemPageView struct {
	SystemID int64
	State    string // "ready" | "missing"
	Name     string
	Security float64 // shown EVE-style by secStatus
	RegionID int64
	Region   string
	Stations []systemStationLine
	// Activity is the distilled activity block; nil until the
	// collector has figures for the system.
	Activity *placeActivityView
	// The sovereignty holder, one of the three kinds ESI reports:
	// an alliance (linked), a corporation (linked), or a faction
	// (name text). All zero/empty when the system is unclaimed or
	// collection has not reached it.
	SovAllianceID   int64
	SovAllianceName string
	SovCorpID       int64
	SovCorpName     string
	SovFactionName  string
}

// stationPageView is the /station/ page body.
type stationPageView struct {
	StationID int64
	State     string // "ready" | "missing"
	Name      string
	SystemID  int64
	System    string
	RegionID  int64
	Region    string
}

// handleSystemPage renders one solar system's SDE record.
func (app *Application) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored rows only

	id, err := strconv.ParseInt(r.URL.Query().Get("system"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.System = app.loadSystemView(ctx, id)
	if s := data.System; s != nil && s.State == "ready" {
		desc := s.Name
		if s.Region != "" {
			desc += " in " + s.Region
		}
		desc += ": security, activity, stations and market presence. EveSynapse EVE Online universe data."
		data.HeadMeta = app.HeadMeta(module.PageMeta{Title: s.Name, Description: desc, CanonicalPath: "/system/"})
	}
	app.render(ctx, w, http.StatusOK, "system.html", data)
}

// regionSystemLine is one system listed on a region page.
type regionSystemLine struct {
	SystemID int64
	Name     string
	Security float64
	// Trailing-24h activity from the distilled stats, formatted;
	// "0" until collection reaches the system.
	Kills24h string
	Jumps24h string
}

// regionSovLine is one holder in a region's sovereignty
// breakdown: an alliance, a corporation, or a faction, with how
// many of the region's systems it holds.
type regionSovLine struct {
	AllianceID   int64
	AllianceName string
	CorpID       int64
	CorpName     string
	FactionName  string
	SystemsLabel string // "12 systems" / "1 system"
}

// regionSovBreakdownLimit bounds the holders a region page lists.
const regionSovBreakdownLimit = 10

func sovSystemsLabel(n int64) string {
	if n == 1 {
		return "1 system"
	}
	return esi.FormatInt(n) + " systems"
}

// regionPageView is the /region/ page body.
type regionPageView struct {
	RegionID int64
	State    string // "ready" | "missing"
	Name     string
	Systems  []regionSystemLine
	// How the region's systems split by security band.
	High, Low, Null int
	// Activity is the region-wide distilled block; nil until the
	// collector has figures for the region.
	Activity *placeActivityView
	// Sov is the sovereignty breakdown, largest holders first,
	// bounded by regionSovBreakdownLimit; SovTruncated says
	// whether more holders exist beyond the list.
	Sov          []regionSovLine
	SovTruncated bool
}

// handleRegionPage renders one region's SDE record: its systems.
func (app *Application) handleRegionPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored rows only

	id, err := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data.Region = app.loadRegionView(ctx, id)
	if v := data.Region; v != nil && v.State == "ready" {
		data.HeadMeta = app.HeadMeta(module.PageMeta{
			Title:         v.Name,
			Description:   v.Name + ": its systems, security and activity across the region. EveSynapse EVE Online universe data.",
			CanonicalPath: "/region/",
		})
	}
	app.render(ctx, w, http.StatusOK, "region.html", data)
}

// loadRegionView builds the region page from the SDE tables.
func (app *Application) loadRegionView(ctx context.Context, id int64) *regionPageView {
	view := &regionPageView{RegionID: id, State: "missing"}
	region, err := app.queries.GetSDERegion(ctx, id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("region page: read region %d: %v", id, err)
		}
		return view
	}
	view.State = "ready"
	view.Name = region.Name
	systems, err := app.queries.ListRegionSystemsWithActivity(ctx, id)
	if err != nil {
		logging.Errorf("region page: list systems for region %d: %v", id, err)
		return view
	}
	for _, sys := range systems {
		view.Systems = append(view.Systems, regionSystemLine{
			SystemID: sys.SystemID, Name: sys.Name, Security: sys.Security,
			Kills24h: esi.FormatInt(sys.ShipKills24h), Jumps24h: esi.FormatInt(sys.Jumps24h),
		})
		switch _, tier := secRound(sys.Security); {
		case tier >= 5:
			view.High++
		case tier >= 1:
			view.Low++
		default:
			view.Null++
		}
	}

	if stats, aerr := app.queries.GetRegionActivityStats(ctx, id); aerr == nil {
		view.Activity = placeActivityViewOf(
			stats.ShipKills1h, stats.PodKills1h, stats.NpcKills1h, stats.Jumps1h,
			stats.ShipKills24h, stats.PodKills24h, stats.NpcKills24h, stats.Jumps24h,
			stats.ShipKillsDay, stats.PodKillsDay, stats.NpcKillsDay, stats.JumpsDay,
			stats.HoursCovered)
	} else if !errors.Is(aerr, sql.ErrNoRows) {
		logging.Errorf("region page: read activity for region %d: %v", id, aerr)
	}

	holders, herr := app.queries.ListRegionSovereigntyBreakdown(ctx, db.ListRegionSovereigntyBreakdownParams{
		RegionID: id, Lim: regionSovBreakdownLimit + 1,
	})
	if herr != nil {
		logging.Errorf("region page: sovereignty breakdown for region %d: %v", id, herr)
		return view
	}
	if len(holders) > regionSovBreakdownLimit {
		view.SovTruncated = true
		holders = holders[:regionSovBreakdownLimit]
	}
	for _, h := range holders {
		line := regionSovLine{SystemsLabel: sovSystemsLabel(h.Systems)}
		switch {
		case h.AllianceID > 0:
			line.AllianceID = h.AllianceID
			line.AllianceName = app.allianceDisplayName(ctx, h.AllianceID)
		case h.CorporationID > 0:
			line.CorpID = h.CorporationID
			line.CorpName = app.corpDisplayName(ctx, h.CorporationID)
		case h.FactionID > 0:
			line.FactionName = h.FactionName
			if line.FactionName == "" {
				line.FactionName = fmt.Sprintf("Faction #%d", h.FactionID)
			}
		default:
			continue
		}
		view.Sov = append(view.Sov, line)
	}
	return view
}

// handleStationPage renders one NPC station's SDE record.
func (app *Application) handleStationPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored rows only

	id, err := strconv.ParseInt(r.URL.Query().Get("station"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data.Station = app.loadStationView(ctx, id)
	if v := data.Station; v != nil && v.State == "ready" {
		data.HeadMeta = app.HeadMeta(module.PageMeta{
			Title:         v.Name,
			Description:   v.Name + ": station services, market and activity. EveSynapse EVE Online universe data.",
			CanonicalPath: "/station/",
		})
	}
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
	view.Security = sys.Security
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

	if stats, aerr := app.queries.GetSystemActivityStats(ctx, id); aerr == nil {
		view.Activity = placeActivityViewOf(
			stats.ShipKills1h, stats.PodKills1h, stats.NpcKills1h, stats.Jumps1h,
			stats.ShipKills24h, stats.PodKills24h, stats.NpcKills24h, stats.Jumps24h,
			stats.ShipKillsDay, stats.PodKillsDay, stats.NpcKillsDay, stats.JumpsDay,
			stats.HoursCovered)
	} else if !errors.Is(aerr, sql.ErrNoRows) {
		logging.Errorf("system page: read activity for system %d: %v", id, aerr)
	}

	if sov, verr := app.queries.GetSystemSovereignty(ctx, id); verr == nil {
		switch {
		case sov.AllianceID > 0:
			view.SovAllianceID = sov.AllianceID
			view.SovAllianceName = app.allianceDisplayName(ctx, sov.AllianceID)
		case sov.CorporationID > 0:
			view.SovCorpID = sov.CorporationID
			view.SovCorpName = app.corpDisplayName(ctx, sov.CorporationID)
		case sov.FactionID > 0:
			view.SovFactionName = sov.FactionName
			if view.SovFactionName == "" {
				view.SovFactionName = fmt.Sprintf("Faction #%d", sov.FactionID)
			}
		}
	} else if !errors.Is(verr, sql.ErrNoRows) {
		logging.Errorf("system page: read sovereignty for system %d: %v", id, verr)
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
		view.RegionID = sys.RegionID
		if region, rerr := app.queries.GetSDERegion(ctx, sys.RegionID); rerr == nil {
			view.Region = region.Name
		}
	}
	return view
}

// linkPlace classifies a rendered location title for the name policy:
// an NPC station links to the station page, a solar system to the
// system page, and a player structure to the structure page once there
// is a resolved name or stored context to show. Anything else stays
// plain text, so a link never lands on a page that cannot render.
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
