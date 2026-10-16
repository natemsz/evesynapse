package app

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public ship pages plus the market sections shared with the
// Item Database type page: SDE identity plus already-warmed
// market data, server-rendered with meta/OG tags. No session
// queries, no want queueing: crawlers must never schedule ESI
// work or read per-user state.
// ---------------------------------------------------------------------------

// publicHubRegions are the market hubs shown on item and ship
// pages, matched by region name so no region ID is hardcoded.
var publicHubRegions = []string{"The Forge", "Domain", "Sinq Laison", "Metropolis", "Heimatar"}

type publicHubPrice struct {
	Region   string
	BestSell string
	BestBuy  string
}

// publicShipView is one /ships/ page: the shared SDE detail, hub
// prices, a history sparkline, and that hull's public fits.
type publicShipView struct {
	Detail      *itemTypeDetail
	Hubs        []publicHubPrice
	History     template.HTML
	HistoryNote string
	Fits        []publicFitCard
	BrowserURL  string
}

// sparkline draws up to 90 daily averages as an inline SVG
// polyline. Empty input draws nothing; a flat series draws a
// midline. Pure server math, no script needed.
func sparkline(points []float64) template.HTML {
	n := len(points)
	if n == 0 {
		return ""
	}
	if n > 90 {
		points = points[len(points)-90:]
		n = 90
	}
	lo, hi := points[0], points[0]
	for _, p := range points[1:] {
		if p < lo {
			lo = p
		}
		if p > hi {
			hi = p
		}
	}
	const width, height = 600.0, 120.0
	var b strings.Builder
	b.WriteString(`<svg class="spark" viewBox="0 0 600 120" role="img" aria-label="Price history">`)
	b.WriteString(`<polyline fill="none" stroke-width="2" points="`)
	span := hi - lo
	for i, p := range points {
		x := width * float64(i) / float64(n-1)
		if n == 1 {
			x = 0
		}
		y := height / 2
		if span > 0 {
			y = height - 8 - (p-lo)/span*(height-16)
		}
		if i > 0 {
			b.WriteString(" ")
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String())
}

// publicTypeMarket loads one type's swept region stats with
// region names resolved in the same read, and derives both the
// named hub rows and The Forge's region ID from it: one stats
// read per render, no per-region lookups.
func (app *Application) publicTypeMarket(ctx context.Context, typeID int64) (hubs []publicHubPrice, forgeRegion int64) {
	rows, err := app.queries.ListPublicHubStats(ctx, typeID)
	if err != nil {
		logging.Errorf("public item %d: hub stats: %v", typeID, err)
		return nil, 0
	}
	want := map[string]bool{}
	for _, name := range publicHubRegions {
		want[name] = true
	}
	for _, row := range rows {
		region := row.RegionName
		if region == "" {
			region = "Region #" + idString(row.RegionID)
		}
		if region == "The Forge" && forgeRegion == 0 {
			forgeRegion = row.RegionID
		}
		if !want[region] {
			continue
		}
		hub := publicHubPrice{Region: region}
		if row.BestSell > 0 {
			hub.BestSell = esi.FormatISK(row.BestSell)
		}
		if row.BestBuy > 0 {
			hub.BestBuy = esi.FormatISK(row.BestBuy)
		}
		if hub.BestSell != "" || hub.BestBuy != "" {
			hubs = append(hubs, hub)
		}
	}
	if forgeRegion == 0 && len(rows) > 0 {
		forgeRegion = rows[0].RegionID
	}
	return hubs, forgeRegion
}

// publicHistorySparkline draws one region's newest recorded
// daily averages, newest first in storage, oldest first on the
// line. Empty when nothing was ever swept for the pair.
func (app *Application) publicHistorySparkline(ctx context.Context, regionID, typeID int64) (template.HTML, string) {
	rows, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: regionID, TypeID: typeID, RowLimit: 90,
	})
	if err != nil {
		logging.Errorf("public item %d: history: %v", typeID, err)
		return "", ""
	}
	if len(rows) == 0 {
		return "", "No price history swept yet."
	}
	points := make([]float64, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		points = append(points, rows[i].Average)
	}
	return sparkline(points), fmt.Sprintf("%d recorded days", len(rows))
}

// handlePublicShip renders one /ships/ page: the hull's SDE
// identity, warmed market data, and its public fits. Non-ships
// 404: group category "Ship" decides, not a hardcoded ID.
func (app *Application) handlePublicShip(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)
	userID := app.userID(ctx)

	typeID, err := strconv.ParseInt(chi.URLParam(r, "typeID"), 10, 64)
	if err != nil || typeID <= 0 {
		http.NotFound(w, r)
		return
	}
	t, err := app.queries.GetSDEType(ctx, typeID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	detail := app.buildItemTypeDetail(ctx, t)
	isShip := detail.Category != nil && detail.Category.Name == "Ship"
	if !isShip {
		http.NotFound(w, r)
		return
	}
	hubs, forge := app.publicTypeMarket(ctx, typeID)
	view := &publicShipView{
		Detail: detail, Hubs: hubs,
		BrowserURL: publicFitsPath + "?hullq=" + url.QueryEscape(detail.Name),
	}
	if forge > 0 {
		view.History, view.HistoryNote = app.publicHistorySparkline(ctx, forge, typeID)
	} else {
		view.HistoryNote = "No price history swept yet."
	}
	// The public fits list is the Public Fits module's surface.
	if moduleCompiledIn("publicfits") {
		rows, err := app.queries.ListPublicFitsForShip(ctx, db.ListPublicFitsForShipParams{ShipTypeID: typeID, Limit: 12})
		if err != nil {
			logging.Errorf("public ship %d: fits: %v", typeID, err)
		} else {
			for _, row := range rows {
				view.Fits = append(view.Fits, publicFitCardOf(db.ListPublicFitsRow(row), userID))
			}
		}
	}
	data.HeadMeta = module.RenderPageMeta(module.PageMeta{
		Title:         detail.Name,
		Description:   detail.Name + " stats, market prices and community fittings in EveSynapse's EVE Online fitting and market tools.",
		CanonicalPath: "/ships/" + idString(typeID) + "/",
	})
	data.PublicShip = view
	app.render(ctx, w, http.StatusOK, "ship_public.html", data)
}
