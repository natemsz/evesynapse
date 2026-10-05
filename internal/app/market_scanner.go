package app

// P2 spread scanner (Element43 parity plan): the station-trading
// screen. It reads only stored rows -- the worker's per-station
// book statistics (market_station_stats, schema 032, filled by
// the same whole-region sweeps as the P1 region stats) plus the
// sweeps' stored 7-day average traded volume per type (schema
// 035) -- and never calls out. Rendering is a couple of SQLite
// reads; all the spread and profit math happens here over stored
// numbers.

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// scannerRowCap caps how many opportunities one render lists.
// The sort is by estimated daily profit, so the cap keeps the
// best trades and drops the long tail.
const scannerRowCap = 100

// defaultScannerMinSpreadPct is the floor the spread filter
// starts at: below this the gap rarely covers fees and effort.
const defaultScannerMinSpreadPct = 5.0

// scannerRow is one opportunity line, display-ready.
type scannerRow struct {
	TypeID      int64
	ItemName    string
	Station     placeRef
	BestBuy     string // esi.FormatISK, highest buy price here
	BestSell    string // esi.FormatISK, lowest sell price here
	SpreadPct   string // "12.3%"
	DailyVolume string // esi.FormatInt, sold per day in the region
	DailyProfit string // esi.FormatISK, estimated daily profit

	// Raw figures behind the formatted strings, kept for the
	// sort (never rendered directly).
	profitRaw float64
}

// scannerView is the /market/scanner/ page body.
type scannerView struct {
	RegionID   int64
	RegionName string
	Regions    []marketRegion
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
// sweep state means the honest still-gathering state; the page
// never triggers a fetch.
func (app *Application) handleMarketScanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	data.MarketScanner = app.buildScannerView(ctx, r.URL.Query())
	app.render(ctx, w, http.StatusOK, "market_scanner.html", data)
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
// spread.
func (app *Application) buildScannerView(ctx context.Context, q map[string][]string) *scannerView {
	regionID := defaultMarketRegion
	if raw := firstQuery(q, "region"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if _, ok := marketRegionName(id); ok {
				regionID = id
			}
		}
	}
	regionName, _ := marketRegionName(regionID)

	minVolume := int64(1)
	if raw := firstQuery(q, "minvol"); raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v >= 0 {
			minVolume = v
		}
	}

	minSpread := defaultScannerMinSpreadPct
	if raw := firstQuery(q, "minspread"); raw != "" {
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
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegion{
			ID: region.ID, Name: region.Name, Active: region.ID == regionID,
		})
	}

	stats, err := app.queries.ListMarketStationStatsByRegion(ctx, regionID)
	if err != nil {
		log.Printf("scanner: list station stats for region %d: %v", regionID, err)
		return view
	}
	if len(stats) == 0 {
		return view // no sweep yet: still-gathering state
	}
	view.HasData = true

	// The freshest row stamps the page: how current the prices
	// a reader is judging are.
	latest := ""
	for _, s := range stats {
		if s.UpdatedAt > latest {
			latest = s.UpdatedAt
		}
	}
	view.Age = statsAgeText(latest)
	if at, perr := time.Parse(time.RFC3339, latest); perr == nil {
		view.AsOf = "Prices as of " + at.Format("Jan 2, 3:04 PM")
	}

	// Daily traded volume per type: the sweep's stored 7-day
	// average from market_region_stats (schema 035), one read
	// for the whole region -- not a history query per type.
	dailyVolume := make(map[int64]float64)
	if regionRows, rerr := app.queries.ListMarketRegionStatsByRegion(ctx, regionID); rerr != nil {
		log.Printf("scanner: list region stats for region %d: %v", regionID, rerr)
	} else {
		for _, s := range regionRows {
			dailyVolume[s.TypeID] = s.AvgDailyVolume
		}
	}

	typeNames := make(map[int64]string)
	itemNameFor := func(typeID int64) string {
		if name, ok := typeNames[typeID]; ok {
			return name
		}
		name := app.typeNameOrID(ctx, typeID)
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

	var rows []scannerRow
	for _, s := range stats {
		if s.BestBuy <= 0 || s.BestSell <= s.BestBuy {
			continue
		}
		spreadPct := (s.BestSell - s.BestBuy) / s.BestBuy * 100
		if spreadPct < minSpread {
			continue
		}
		vol := dailyVolume[s.TypeID]
		if vol < float64(minVolume) {
			continue
		}
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
		if size <= 0 {
			continue
		}
		profit := (s.BestSell - s.BestBuy) * size
		if profit <= 0 {
			continue
		}
		rows = append(rows, scannerRow{
			TypeID:      s.TypeID,
			ItemName:    itemNameFor(s.TypeID),
			Station:     app.scannerStationRef(ctx, s.LocationID),
			BestBuy:     esi.FormatISK(s.BestBuy),
			BestSell:    esi.FormatISK(s.BestSell),
			SpreadPct:   fmt.Sprintf("%.1f%%", spreadPct),
			DailyVolume: esi.FormatInt(int64(math.Round(vol))),
			DailyProfit: esi.FormatISK(profit),
			profitRaw:   profit,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].profitRaw != rows[j].profitRaw {
			return rows[i].profitRaw > rows[j].profitRaw
		}
		if rows[i].TypeID != rows[j].TypeID {
			return rows[i].TypeID < rows[j].TypeID
		}
		return rows[i].Station.Name < rows[j].Station.Name
	})
	if len(rows) > scannerRowCap {
		rows = rows[:scannerRowCap]
	}
	view.Rows = rows
	return view
}

// firstQuery returns the first value of a query parameter.
func firstQuery(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return strings.TrimSpace(vs[0])
	}
	return ""
}

// scannerStationRef resolves one order location to its display
// title and link by the name policy: an NPC station the SDE
// knows links to the station page, a player structure with a
// resolved name links to the structure page, and a structure
// nobody has named yet reads "Player structure" as plain text
// -- never a bare number. Cache-only.
func (app *Application) scannerStationRef(ctx context.Context, locationID int64) placeRef {
	if locationID <= 0 {
		return placeRef{Name: "Player structure"}
	}
	if isStructureID(locationID) {
		if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
			return app.linkPlace(ctx, locationID, name)
		}
		return placeRef{Name: "Player structure"}
	}
	title := app.econLocationTitle(ctx, locationID)
	if strings.HasPrefix(title, "Station #") || strings.HasPrefix(title, "Structure #") {
		// The SDE has no name for this place; keep the promise
		// that the scanner never shows a bare number.
		title = "Player structure"
		return placeRef{Name: title}
	}
	return app.linkPlace(ctx, locationID, title)
}
