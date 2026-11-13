package scanner

// Spread scanner: the station-trading screen. It reads only stored rows
//, the per-station book statistics (market_station_stats) and the
// stored 7-day average traded volume per type, both filled by the
// whole-region sweeps. The filter math, profit estimate and ranking
// happen in one SQL query capped at rowCap rows, so a page load
// costs the same however many rows a region holds.

import (
	"context"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/tradefees"
)

// scannerRow is one opportunity line, display-ready.
type scannerRow struct {
	TypeID      int64
	ItemName    string
	Station     module.PlaceRef
	TypicalBuy  string // esi.FormatISK, median buy price here
	TypicalSell string // esi.FormatISK, median sell price here
	SpreadPct   string // "12.3%", net of broker fees and sales tax
	SpreadEst   bool   // the station's fee is not on file, so the net spread used the NPC formula as an estimate
	DailyVolume string // esi.FormatInt, sold per day in the region
	DailyProfit string // esi.FormatISK, estimated daily profit
	// The station system's security, for the badge (SecKnown false
	// when the SDE has no row for it).
	SecText  string
	SecTier  int
	SecKnown bool
	// SpreadW and ProfitW fill the row's meters: the spread and the
	// daily profit as a percent of the best on the page.
	SpreadW int
	ProfitW int
	// Rank is the row's place in the list (1 is the best daily
	// profit); PickHref opens it in the inspector, Picked marks the
	// open one.
	Rank     int
	PickHref string
	Picked   bool

	spread, profit float64
}

// scannerView is the /market/scanner/ page body.
type scannerView struct {
	RegionID   int64
	RegionName string
	Regions    []module.RegionChoice
	MinVolume  int64  // sold-per-day floor from the filter
	MinSpread  string // spread-% floor as typed (echoed in the form)
	Scope      string // the raw ?scope= value, carried by the filter form
	Selector   *module.ScopeSelector
	HasData    bool   // a sweep has covered this region
	AsOf       string // "Prices as of Oct 4, 3:04 PM", HasData only
	Age        string // "12 minutes ago", HasData only
	Rows       []scannerRow
	// Pick is the opportunity open in the inspector (?pick=<rank>),
	// nil for none; Depth is the console level it puts the page at.
	// CloseHref returns to the list with the same filters.
	Pick      *scannerRow
	Depth     int
	CloseHref string
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
	header := []string{"Item", "Station", "Typical buy price", "Typical sell price", "Spread %", "Sold per day", "Estimated daily profit"}
	rows := make([][]string, 0, len(view.Rows))
	for _, row := range view.Rows {
		rows = append(rows, []string{
			row.ItemName,
			row.Station.Name,
			row.TypicalBuy,
			row.TypicalSell,
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
// whichever is smallest. Rows are priced typical against
// typical (the sweep's stored station medians), need a book
// with depth on both sides, must sit within the region's own
// typical range, and need enough daily trades and a
// wide-enough spread. The SQL pre-filter (gross spread at or
// over the floor, capped at rowCap rows) is a superset of the
// net filter: net is always at or under gross, so no row the
// net filter would keep is ever cut upstream. Go then nets
// each row at the station's rates for the viewer, drops the
// rows whose net spread falls under the floor, and re-ranks
// by net daily profit.
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
		Scope:      module.QueryValue(q, "scope"),
	}
	// The pricer resolves the viewer once for the render: the
	// scope control above the list, and the per-station rates
	// below. Anonymous readers get perfect skills and relations.
	pricer := app.deps.TradePricer(ctx, q, "/market/scanner/")
	view.Selector = pricer.ScopeControl()
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
		// The spread and profit are net of broker fees and
		// sales tax at this station, for the viewer. A row
		// whose net spread falls under the floor drops out:
		// the SQL floor is on the gross gap, so a thin net
		// gap can still arrive here.
		broker, salesTax, estimated := pricer.Rates(ctx, s.LocationID)
		spreadPct := tradefees.NetSpreadPct(s.TypicalBuy, s.TypicalSell, broker, salesTax)
		if spreadPct < minSpread {
			continue
		}
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
		profit, _ := tradefees.NetRoundTrip(s.TypicalBuy, s.TypicalSell, broker, salesTax)
		profit *= size
		station := app.deps.StationRef(ctx, stationMemo, s.LocationID)
		secText, secTier, secKnown := "", 0, false
		if station.SystemID != 0 {
			secText, secTier, secKnown = app.deps.SystemSecurity(ctx, station.SystemID)
		}
		view.Rows = append(view.Rows, scannerRow{
			SecText:     secText,
			SecTier:     secTier,
			SecKnown:    secKnown,
			spread:      spreadPct,
			profit:      profit,
			TypeID:      s.TypeID,
			ItemName:    itemNameFor(s.TypeID),
			Station:     station,
			TypicalBuy:  esi.FormatISK(s.TypicalBuy),
			TypicalSell: esi.FormatISK(s.TypicalSell),
			SpreadPct:   fmt.Sprintf("%.1f%%", spreadPct),
			SpreadEst:   estimated,
			DailyVolume: esi.FormatInt(int64(math.Round(vol))),
			DailyProfit: esi.FormatISK(profit),
		})
	}
	// Net re-ranks: the SQL ranked by gross profit, and a row
	// with a wide gross gap can net worse than a narrower one.
	sort.SliceStable(view.Rows, func(i, j int) bool { return view.Rows[i].profit > view.Rows[j].profit })
	scannerMeters(view, q)
	return view
}

// scannerMeters fills each row's meters against the page's best spread
// and best daily profit, numbers the rows, and opens the one ?pick=
// names in the inspector. The pick and close links keep the filters.
func scannerMeters(view *scannerView, q map[string][]string) {
	bestSpread, bestProfit := 0.0, 0.0
	for _, row := range view.Rows {
		bestSpread = max(bestSpread, row.spread)
		bestProfit = max(bestProfit, row.profit)
	}
	base := url.Values{}
	base.Set("region", strconv.FormatInt(view.RegionID, 10))
	base.Set("minvol", strconv.FormatInt(view.MinVolume, 10))
	base.Set("minspread", view.MinSpread)
	if view.Scope != "" {
		base.Set("scope", view.Scope)
	}
	view.CloseHref = "/market/scanner/?" + base.Encode()
	pick, _ := strconv.Atoi(module.QueryValue(q, "pick"))
	view.Depth = 1 // the list is the top level: there is no directory
	for i := range view.Rows {
		row := &view.Rows[i]
		row.Rank = i + 1
		// Both meters measure against the page's best. A joke order or
		// a thin book can quote a spread in the thousands and leave the
		// other spread meters near empty; filtering those orders out
		// belongs upstream, in the sweep, not in the meter.
		if bestSpread > 0 {
			row.SpreadW = int(math.Round(row.spread / bestSpread * 100))
		}
		if bestProfit > 0 {
			row.ProfitW = int(math.Round(row.profit / bestProfit * 100))
		}
		v := url.Values{}
		maps.Copy(v, base)
		v.Set("pick", strconv.Itoa(row.Rank))
		row.PickHref = "/market/scanner/?" + v.Encode()
		if row.Rank == pick {
			row.Picked = true
			view.Pick = row
			view.Depth = 2
		}
	}
}
