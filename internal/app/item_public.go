package app

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Public item and ship pages: SDE identity plus already-warmed
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

// publicItemView is one /item/ page: the shared SDE detail, hub
// prices from swept stats, and a server-drawn history sparkline.
type publicItemView struct {
	Detail      *itemTypeDetail
	Hubs        []publicHubPrice
	History     template.HTML
	HistoryNote string
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

// publicHubPrices resolves one type's swept region stats to named
// hub rows: one bounded read plus one region-name read per region
// present (a dozen at most on a warm instance).
func (app *Application) publicHubPrices(ctx context.Context, typeID int64) []publicHubPrice {
	rows, err := app.queries.ListMarketRegionStatsByType(ctx, typeID)
	if err != nil {
		logging.Errorf("public item %d: hub stats: %v", typeID, err)
		return nil
	}
	want := map[string]bool{}
	for _, name := range publicHubRegions {
		want[name] = true
	}
	var hubs []publicHubPrice
	for _, row := range rows {
		region := "Region #" + idString(row.RegionID)
		if reg, err := app.queries.GetSDERegion(ctx, row.RegionID); err == nil && reg.Name != "" {
			region = reg.Name
		} else if err != nil {
			logging.Errorf("public item %d: region %d: %v", typeID, row.RegionID, err)
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
	return hubs
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

// publicForgeRegion finds The Forge's region ID from the warmed
// stats, so history reads need no hardcoded region.
func (app *Application) publicForgeRegion(ctx context.Context, typeID int64) int64 {
	rows, err := app.queries.ListMarketRegionStatsByType(ctx, typeID)
	if err != nil {
		return 0
	}
	for _, row := range rows {
		if reg, err := app.queries.GetSDERegion(ctx, row.RegionID); err == nil && reg.Name == "The Forge" {
			return row.RegionID
		}
	}
	if len(rows) > 0 {
		return rows[0].RegionID
	}
	return 0
}

// handlePublicItem renders one /item/ page from SDE plus warmed
// market data. Unknown types 404; nothing here queues ESI work.
func (app *Application) handlePublicItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx)

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
	view := &publicItemView{Detail: detail, Hubs: app.publicHubPrices(ctx, typeID)}
	if forge := app.publicForgeRegion(ctx, typeID); forge > 0 {
		view.History, view.HistoryNote = app.publicHistorySparkline(ctx, forge, typeID)
	} else {
		view.HistoryNote = "No price history swept yet."
	}
	pageURL := app.siteOrigin(r) + "/item/" + idString(typeID) + "/"
	blurb := detail.Name
	if detail.Group != nil {
		blurb += ", " + detail.Group.Name
	}
	if detail.AdjustedPrice != "" {
		blurb += " - " + detail.AdjustedPrice + " ISK"
	}
	data.HeadMeta = headMeta(detail.Name, blurb, pageURL)
	data.PublicItem = view
	app.render(ctx, w, http.StatusOK, "item_public.html", data)
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
	view := &publicShipView{
		Detail: detail, Hubs: app.publicHubPrices(ctx, typeID),
		BrowserURL: publicFitsPath + "?hullq=" + url.QueryEscape(detail.Name),
	}
	if forge := app.publicForgeRegion(ctx, typeID); forge > 0 {
		view.History, view.HistoryNote = app.publicHistorySparkline(ctx, forge, typeID)
	} else {
		view.HistoryNote = "No price history swept yet."
	}
	rows, err := app.queries.ListPublicFitsForShip(ctx, db.ListPublicFitsForShipParams{ShipTypeID: typeID, Limit: 12})
	if err != nil {
		logging.Errorf("public ship %d: fits: %v", typeID, err)
	} else {
		for _, row := range rows {
			var doc fitDoc
			tags := []string{}
			if json.Unmarshal([]byte(row.ItemsJson), &doc) == nil {
				tags = doc.Tags
			}
			card := publicFitCard{
				ID: row.ID, Name: row.Name, Ship: detail.Name, ShipTypeID: typeID,
				Author: row.AuthorName, Mine: userID > 0 && row.UserID == userID,
				Views: row.ViewCount, Price: row.TotalPrice, PriceKnown: row.TotalPrice > 0,
				Updated: row.UpdatedAt, UpdatedLine: rfc3339(row.UpdatedAt),
				FitURL: publicFitsPath + "fit/" + idString(row.ID) + "/", Tags: tags,
			}
			if card.PriceKnown {
				card.PriceLine = esi.FormatISK(row.TotalPrice)
			}
			if row.RatingCount > 0 {
				card.Stars = float64(row.RatingSum) / float64(row.RatingCount)
				card.StarCount = row.RatingCount
			}
			view.Fits = append(view.Fits, card)
		}
	}
	pageURL := app.siteOrigin(r) + "/ships/" + idString(typeID) + "/"
	blurb := detail.Name + " with " + strconv.Itoa(len(view.Fits)) + " public fits"
	data.HeadMeta = headMeta(detail.Name, blurb, pageURL)
	data.PublicShip = view
	app.render(ctx, w, http.StatusOK, "ship_public.html", data)
}
