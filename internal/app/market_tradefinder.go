package app

// P3 Tradefinder (Element43 parity plan): the cross-region route
// screen. It reads only stored rows -- the worker's per-(region,
// type) book statistics (market_region_stats, schema 031) for both
// ends of the route, the per-(station, type) bests
// (market_station_stats, schema 032) for the best-price hints,
// and the sweeps' stored 7-day average traded volume at the
// destination (schema 035) for the sold-per-day figure. It
// never calls out: rendering is a few small bounded reads --
// the route filtering, margin math, and ranking all happen in
// one SQL query capped at tradefinderRowCap rows, so a page
// load costs the same whether the regions hold ten thousand
// or a hundred thousand stored rows.
//
// Formula (deliberate; mirrored in the page copy):
//
//	margin per item  = destination typical sell - origin typical buy
//	margin %         = margin / origin typical buy * 100
//	units per day    = min(destination sold per day,
//	                       origin's open buy volume,
//	                       destination's open sell volume)
//	profit per day   = margin * units per day
//
// The origin's open buy volume is how much buying is already posted
// where the buying happens, and the destination's open sell volume
// is how much stock is already posted where the selling happens --
// both are honest "how much room is there" caps next to what
// actually trades each day. A row with any of the three at zero
// cannot move, so it drops out instead of showing a fantasy number.
// Figures older than three days on EITHER side are excluded: a
// route priced off a stale book is not a route. Routes out of a
// degenerate origin buy book (lowballRoute) are hidden unless the
// viewer opts in: real, but not trade routes.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// tradefinderRowCap caps how many routes one render lists. The
// sort is by estimated daily profit, so the cap keeps the best
// routes and drops the long tail.
const tradefinderRowCap = 100

// defaultTradefinderMinMarginPct is the floor the margin filter
// starts at: below this the gap rarely survives the trip.
const defaultTradefinderMinMarginPct = 5.0

// tradefinderMaxStatAge is the freshness rule: stats older than
// this on either side of a route exclude the route.
const tradefinderMaxStatAge = 3 * 24 * time.Hour

// defaultTradefinderDestination is the default sell-in region:
// Domain (Amarr), the biggest hub outside The Forge.
const defaultTradefinderDestination = int64(10000043)

// tradefinderRow is one route line, display-ready.
type tradefinderRow struct {
	TypeID      int64
	ItemName    string
	BuyTypical  string // esi.FormatISK, origin typical buy
	SellTypical string // esi.FormatISK, destination typical sell
	ProfitItem  string // esi.FormatISK, margin per item
	MarginPct   string // "12.3%"
	SoldPerDay  string // esi.FormatInt, sold per day at destination
	ProfitDay   string // esi.FormatISK, estimated daily profit

	// Best-price hints from the station grain: where the cheapest
	// sell order in the origin and the highest buy order in the
	// destination actually sit. Empty name = no such order.
	OriginStation placeRef
	OriginBest    string // esi.FormatISK, "" when no sell order
	DestStation   placeRef
	DestBest      string // esi.FormatISK, "" when no buy order
}

// tradefinderView is the /market/tradefinder/ page body.
type tradefinderView struct {
	OriginID      int64
	OriginName    string
	DestID        int64
	DestName      string
	Regions       []marketRegion
	MinMargin     string // margin-% floor as typed (echoed in the form)
	MinVolume     int64  // sold-per-day floor from the filter
	Lowball       bool   // include degenerate-book routes (hidden by default)
	SameRegion    bool   // origin == destination: a valid but empty case
	OriginHasData bool   // a sweep has covered the origin region
	DestHasData   bool   // a sweep has covered the destination region
	OriginAsOf    string // "<region> figures last gathered Oct 4, 3:04 PM", when gathered
	DestAsOf      string
	Rows          []tradefinderRow
}

// HasData reports whether both ends have any stored figures at
// all. Either end empty is the still-gathering state, never a
// table of nothing.
func (v *tradefinderView) HasData() bool {
	return v != nil && v.OriginHasData && v.DestHasData
}

// handleMarketTradefinder renders the tradefinder from stored
// rows only. Filters ride the query string (origin, destination,
// minmargin, minvol) so a view is shareable and a reload is free.
// No sweep state means the honest still-gathering state; the page
// never triggers a fetch.
func (app *Application) handleMarketTradefinder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	view := app.buildTradefinderView(ctx, r.URL.Query())
	if wantCSV(r) {
		serveTradefinderCSV(w, view)
		return
	}
	data.MarketTradefinder = view
	app.render(ctx, w, http.StatusOK, "market_tradefinder.html", data)
}

// serveTradefinderCSV writes the tradefinder's route rows as a CSV download.
func serveTradefinderCSV(w http.ResponseWriter, view *tradefinderView) {
	header := []string{"Item", "Buy region", "Buy typical", "Sell region", "Sell typical", "Profit per item", "Margin %", "Sold per day", "Profit per day", "Cheapest origin station", "Origin best", "Best dest station", "Dest best"}
	rows := make([][]string, 0, len(view.Rows))
	for _, row := range view.Rows {
		rows = append(rows, []string{
			row.ItemName,
			view.OriginName,
			row.BuyTypical,
			view.DestName,
			row.SellTypical,
			row.ProfitItem,
			row.MarginPct,
			row.SoldPerDay,
			row.ProfitDay,
			row.OriginStation.Name,
			row.OriginBest,
			row.DestStation.Name,
			row.DestBest,
		})
	}
	serveCSV(w, "tradefinder", header, rows)
}

// buildTradefinderView assembles the tradefinder page for one
// origin/destination pair from stored region stats. Daily volume
// at the destination comes from the sweep's stored per-type
// average (market_region_stats, schema 035) -- ESI's recorded
// daily trades, averaged at sweep time over the newest seven
// recorded days (the same window the spread scanner uses).
func (app *Application) buildTradefinderView(ctx context.Context, q map[string][]string) *tradefinderView {
	originID := defaultMarketRegion
	if raw := firstQuery(q, "origin"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if _, ok := marketRegionName(id); ok {
				originID = id
			}
		}
	}
	destID := defaultTradefinderDestination
	if raw := firstQuery(q, "destination"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if _, ok := marketRegionName(id); ok {
				destID = id
			}
		}
	}
	originName, _ := marketRegionName(originID)
	destName, _ := marketRegionName(destID)

	minMargin := defaultTradefinderMinMarginPct
	if raw := firstQuery(q, "minmargin"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && v >= 0 {
			minMargin = v
		}
	}
	minVolume := int64(1)
	if raw := firstQuery(q, "minvol"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			minVolume = v
		}
	}
	// Lowball routes (degenerate origin buy books) stay hidden
	// unless the viewer opts in; the flag echoes in the form.
	includeLowball := firstQuery(q, "lowball") == "1"

	view := &tradefinderView{
		OriginID:   originID,
		OriginName: originName,
		DestID:     destID,
		DestName:   destName,
		SameRegion: originID == destID,
		MinMargin:  strconv.FormatFloat(minMargin, 'f', -1, 64),
		MinVolume:  minVolume,
		Lowball:    includeLowball,
	}
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegion{
			ID: region.ID, Name: region.Name,
		})
	}

	originStats, err := app.queries.GetMarketRegionStatsStamp(ctx, originID)
	if err != nil {
		logging.Errorf("tradefinder: region stats stamp for origin %d: %v", originID, err)
		return view
	}
	destStats, err := app.queries.GetMarketRegionStatsStamp(ctx, destID)
	if err != nil {
		logging.Errorf("tradefinder: region stats stamp for destination %d: %v", destID, err)
		return view
	}
	view.OriginHasData = originStats.RowCount > 0
	view.DestHasData = destStats.RowCount > 0
	if at, ok := originStats.Stamp.(time.Time); ok {
		view.OriginAsOf = "figures last gathered " + at.UTC().Format("Jan 2, 3:04 PM")
	}
	if at, ok := destStats.Stamp.(time.Time); ok {
		view.DestAsOf = "figures last gathered " + at.UTC().Format("Jan 2, 3:04 PM")
	}
	if !view.HasData() || view.SameRegion {
		return view // still-gathering state, or the same-region empty case
	}

	typeNames := make(map[int64]string)
	itemNameFor := func(typeID int64) string {
		if name, ok := typeNames[typeID]; ok {
			return name
		}
		name := app.typeNameOrID(ctx, typeID)
		if strings.HasPrefix(name, "Type #") {
			if row, rerr := app.queries.GetSDEType(ctx, typeID); rerr == nil && row.Name != "" {
				name = row.Name
			}
		}
		typeNames[typeID] = name
		return name
	}

	// The route math, margin math, freshness rule, and ranking
	// all happen in one bounded SQL query (ListTradefinderRoutes);
	// Go only formats the displayed rows.
	lowballParam := int64(0)
	if includeLowball {
		lowballParam = 1
	}
	routes, err := app.queries.ListTradefinderRoutes(ctx, db.ListTradefinderRoutesParams{
		DestRegion:     destID,
		OriginRegion:   originID,
		IncludeLowball: lowballParam,
		MinMargin:      minMargin,
		MinVolume:      minVolume,
		Cutoff:         time.Now().UTC().Add(-tradefinderMaxStatAge),
		RowCap:         int64(tradefinderRowCap),
	})
	if err != nil {
		logging.Errorf("tradefinder: list routes %d -> %d: %v", originID, destID, err)
		return view
	}

	// Best-price hints: the cheapest sell station in the origin
	// and the highest buy station in the destination, resolved
	// only for the displayed routes -- never the whole region.
	originCheapest := make(map[int64]db.ListCheapestSellStationsRow, len(routes))
	destBestBuy := make(map[int64]db.ListBestBuyStationsRow, len(routes))
	if len(routes) > 0 {
		typeIDs := make([]int64, 0, len(routes))
		for _, r := range routes {
			typeIDs = append(typeIDs, r.TypeID)
		}
		if hints, herr := app.queries.ListCheapestSellStations(ctx, db.ListCheapestSellStationsParams{RegionID: originID, TypeIds: typeIDs}); herr != nil {
			logging.Errorf("tradefinder: cheapest-sell hints for region %d: %v", originID, herr)
		} else {
			for _, h := range hints {
				originCheapest[h.TypeID] = h
			}
		}
		if hints, herr := app.queries.ListBestBuyStations(ctx, db.ListBestBuyStationsParams{RegionID: destID, TypeIds: typeIDs}); herr != nil {
			logging.Errorf("tradefinder: best-buy hints for region %d: %v", destID, herr)
		} else {
			for _, h := range hints {
				destBestBuy[h.TypeID] = h
			}
		}
	}

	stationMemo := make(map[int64]placeRef)
	for _, r := range routes {
		margin := r.DestTypicalSell - r.OriginTypicalBuy
		marginPct := margin / r.OriginTypicalBuy * 100
		// Sold per day at the destination: the sweep's stored
		// 7-day average on the destination's region row (schema
		// 035) -- no per-type history reads at render time.
		vol := r.DestDailyVolume
		// The movable size is whichever runs out first: what
		// actually trades at the destination each day, the open
		// buy volume where the buying happens, or the open sell
		// volume where the selling happens.
		size := vol
		if float64(r.OriginBuyVolume) < size {
			size = float64(r.OriginBuyVolume)
		}
		if float64(r.DestSellVolume) < size {
			size = float64(r.DestSellVolume)
		}
		profit := margin * size
		row := tradefinderRow{
			TypeID:      r.TypeID,
			ItemName:    itemNameFor(r.TypeID),
			BuyTypical:  esi.FormatISK(r.OriginTypicalBuy),
			SellTypical: esi.FormatISK(r.DestTypicalSell),
			ProfitItem:  esi.FormatISK(margin),
			MarginPct:   fmt.Sprintf("%.1f%%", marginPct),
			SoldPerDay:  esi.FormatInt(int64(math.Round(vol))),
			ProfitDay:   esi.FormatISK(profit),
		}
		if hint, ok := originCheapest[r.TypeID]; ok {
			row.OriginStation = app.scannerStationRef(ctx, stationMemo, hint.LocationID)
			row.OriginBest = esi.FormatISK(hint.BestSell)
		}
		if hint, ok := destBestBuy[r.TypeID]; ok {
			row.DestStation = app.scannerStationRef(ctx, stationMemo, hint.LocationID)
			row.DestBest = esi.FormatISK(hint.BestBuy)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}
