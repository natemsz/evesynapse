package scanner

// Spread scanner: the station-trading screen. It reads only stored rows
// — the per-station book statistics (market_station_stats) and the
// stored 7-day average traded volume per type, both filled by the
// whole-region sweeps. The filter math, profit estimate and ranking
// happen in one SQL query capped at rowCap rows, so a page load
// costs the same however many rows a region holds.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// scannerRow is one opportunity line, display-ready.
type scannerRow struct {
	TypeID      int64
	ItemName    string
	Station     module.PlaceRef
	BestBuy     string // esi.FormatISK, highest buy price here
	BestSell    string // esi.FormatISK, lowest sell price here
	SpreadPct   string // "12.3%"
	DailyVolume string // esi.FormatInt, sold per day in the region
	DailyProfit string // esi.FormatISK, estimated daily profit
}

// scannerView is the /market/scanner/ page body.
type scannerView struct {
	RegionID   int64
	RegionName string
	Regions    []module.RegionChoice
	MinVolume  int64  // sold-per-day floor from the filter
	MinSpread  string // spread-% floor as typed (echoed in the form)
	HasData    bool   // a sweep has covered this region
	AsOf       string // "Prices as of Oct 4, 3:04 PM", HasData only
	Age        string // "12 minutes ago", HasData only
	Rows       []scannerRow
}

// handleMarketScanner renders the spread scanner from stored
// rows only. Filters ride the query string (region, minvol,
// minspread) so a view is shareable and a reload is free. No
// sweep state means the still-gathering state; the page
// never triggers a fetch.
func (app *page) handleMarketScanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PublicShell(ctx)} // public: stored market data only
	view := app.buildScannerView(ctx, r.URL.Query())
	if r.URL.Query().Get("format") == "csv" {
		app.serveScannerCSV(w, view)
		return
	}
	data.MarketScanner = view
	app.deps.Render(ctx, w, http.StatusOK, "market_scanner.html", data)
}

// serveScannerCSV writes the scanner's opportunity rows as a CSV download.
func (app *page) serveScannerCSV(w http.ResponseWriter, view *scannerView) {
	header := []string{"Item", "Station", "Highest buy price", "Lowest sell price", "Spread %", "Sold per day", "Estimated daily profit"}
	rows := make([][]string, 0, len(view.Rows))
	for _, row := range view.Rows {
		rows = append(rows, []string{
			row.ItemName,
			row.Station.Name,
			row.BestBuy,
			row.BestSell,
			row.SpreadPct,
			row.DailyVolume,
			row.DailyProfit,
		})
	}
	app.deps.ServeCSV(w, "spread-scanner", header, rows)
}

// buildScannerView assembles the scanner page for one region
// from stored station stats. Daily volume comes from the
// sweep's stored per-type average (market_region_stats,
// schema 035) -- ESI's recorded daily trades, averaged at
// sweep time over the newest seven recorded days (the same
// window the item page's trading snapshot uses). It is a
// liquidity proxy: the column is labelled "sold per day in
// <region>" and the profit estimate is capped by what
// actually trades, what sellers hold, and what buyers want,
// whichever is smallest. Rows need a real buy above zero, a
// sell above the buy, enough daily trades, and a wide-enough
// spread. The filtering, profit math, and ranking all happen
// in one bounded SQL query (ListScannerOpportunities); Go
// only formats the displayed rows.
func (app *page) buildScannerView(ctx context.Context, q map[string][]string) *scannerView {
	regionID := app.deps.DefaultMarketRegion()
	if raw := module.QueryValue(q, "region"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if _, ok := app.deps.MarketRegionName(id); ok {
				regionID = id
			}
		}
	}
	regionName, _ := app.deps.MarketRegionName(regionID)

	minVolume := int64(1)
	if raw := module.QueryValue(q, "minvol"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			minVolume = v
		}
	}

	defaultMinSpread, rowCap := app.deps.SpreadDefaults()
	minSpread := defaultMinSpread
	if raw := module.QueryValue(q, "minspread"); raw != "" {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && !math.IsNaN(v) && v >= 0 {
			minSpread = v
		}
	}

	view := &scannerView{
		RegionID:   regionID,
		RegionName: regionName,
		MinVolume:  minVolume,
		MinSpread:  strconv.FormatFloat(minSpread, 'f', -1, 64),
	}
	for _, region := range app.deps.MarketRegions() {
		view.Regions = append(view.Regions, module.RegionChoice{
			ID: region.ID, Name: region.Name, Active: region.ID == regionID,
		})
	}

	// The freshest station-stat write stamps the page: how
	// current the prices a reader is judging are. No rows at
	// all means the sweep hasn't covered this region yet.
	stampRow, err := app.queries.GetMarketStationStatsStamp(ctx, regionID)
	if err != nil {
		logging.Errorf("scanner: station stats stamp for region %d: %v", regionID, err)
		return view
	}
	if stampRow.RowCount == 0 {
		return view // no sweep yet: still-gathering state
	}
	view.HasData = true
	if latest, ok := stampRow.Stamp.(time.Time); ok {
		view.Age = app.deps.AgeText(latest)
		view.AsOf = "Prices as of " + latest.UTC().Format("Jan 2, 3:04 PM")
	}

	typeNames := make(map[int64]string)
	itemNameFor := func(typeID int64) string {
		if name, ok := typeNames[typeID]; ok {
			return name
		}
		name := app.deps.TypeNameOrID(ctx, typeID)
		if strings.HasPrefix(name, "Type #") {
			// The SDE knows most types by name; fall back to the
			// local SDE row before settling for the placeholder.
			if row, rerr := app.queries.GetSDEType(ctx, typeID); rerr == nil && row.Name != "" {
				name = row.Name
			}
		}
		typeNames[typeID] = name
		return name
	}

	opp, err := app.queries.ListScannerOpportunities(ctx, db.ListScannerOpportunitiesParams{
		RegionID:  regionID,
		MinSpread: minSpread,
		MinVolume: minVolume,
		RowCap:    int64(rowCap),
	})
	if err != nil {
		logging.Errorf("scanner: list opportunities for region %d: %v", regionID, err)
		return view
	}
	stationMemo := make(map[int64]module.PlaceRef)
	for _, s := range opp {
		spreadPct := (s.BestSell - s.BestBuy) / s.BestBuy * 100
		vol := s.DailyVolume
		// The tradeable size is whichever runs out first: what
		// actually trades each day, what sellers hold here, or
		// what buyers want here.
		size := vol
		if float64(s.SellVolume) < size {
			size = float64(s.SellVolume)
		}
		if float64(s.BuyVolume) < size {
			size = float64(s.BuyVolume)
		}
		profit := (s.BestSell - s.BestBuy) * size
		view.Rows = append(view.Rows, scannerRow{
			TypeID:      s.TypeID,
			ItemName:    itemNameFor(s.TypeID),
			Station:     app.deps.StationRef(ctx, stationMemo, s.LocationID),
			BestBuy:     esi.FormatISK(s.BestBuy),
			BestSell:    esi.FormatISK(s.BestSell),
			SpreadPct:   fmt.Sprintf("%.1f%%", spreadPct),
			DailyVolume: esi.FormatInt(int64(math.Round(vol))),
			DailyProfit: esi.FormatISK(profit),
		})
	}
	return view
}
