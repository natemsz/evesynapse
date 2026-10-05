package app

// P3 Tradefinder (Element43 parity plan): the cross-region route
// screen. It reads only stored rows -- the worker's per-(region,
// type) book statistics (market_region_stats, schema 031) for both
// ends of the route, the per-(station, type) bests
// (market_station_stats, schema 032) for the best-price hints,
// and the stored daily trade history (market_history) for the
// destination's sold-per-day figure. It never calls out: rendering
// is a handful of SQLite reads and all the margin math happens
// here over stored numbers.
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
// route priced off a stale book is not a route.

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

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
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

	// Raw figures behind the formatted strings, kept for the sort.
	profitRaw float64
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
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	data.MarketTradefinder = app.buildTradefinderView(ctx, r.URL.Query())
	app.render(ctx, w, http.StatusOK, "market_tradefinder.html", data)
}

// buildTradefinderView assembles the tradefinder page for one
// origin/destination pair from stored region stats. Daily volume
// at the destination comes from market_history -- ESI's recorded
// daily trades, averaged over the newest seven recorded days (the
// same window the spread scanner uses).
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

	view := &tradefinderView{
		OriginID:   originID,
		OriginName: originName,
		DestID:     destID,
		DestName:   destName,
		SameRegion: originID == destID,
		MinMargin:  strconv.FormatFloat(minMargin, 'f', -1, 64),
		MinVolume:  minVolume,
	}
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegion{
			ID: region.ID, Name: region.Name,
		})
	}

	originStats, err := app.queries.ListMarketRegionStatsByRegion(ctx, originID)
	if err != nil {
		log.Printf("tradefinder: list region stats for origin %d: %v", originID, err)
		return view
	}
	destStats, err := app.queries.ListMarketRegionStatsByRegion(ctx, destID)
	if err != nil {
		log.Printf("tradefinder: list region stats for destination %d: %v", destID, err)
		return view
	}
	view.OriginHasData = len(originStats) > 0
	view.DestHasData = len(destStats) > 0
	if at := latestStatStamp(originStats); at != "" {
		view.OriginAsOf = "figures last gathered " + at
	}
	if at := latestStatStamp(destStats); at != "" {
		view.DestAsOf = "figures last gathered " + at
	}
	if !view.HasData() || view.SameRegion {
		return view // still-gathering state, or the same-region empty case
	}

	destByType := make(map[int64]db.MarketRegionStat, len(destStats))
	for _, s := range destStats {
		destByType[s.TypeID] = s
	}

	// Best-price hints: the cheapest sell station in the origin
	// and the highest buy station in the destination, per type,
	// from the station grain the sweeps already store.
	originCheapest := cheapestSellByType(app.listStationStats(ctx, originID))
	destBestBuy := bestBuyByType(app.listStationStats(ctx, destID))

	// Daily traded volume per type at the destination, averaged
	// over the newest seven recorded days. One history read per
	// candidate type, cached across that type's row.
	dailyVolume := make(map[int64]float64)
	dailyVolumeFor := func(typeID int64) float64 {
		if v, ok := dailyVolume[typeID]; ok {
			return v
		}
		rows := app.recentHistoryRows(ctx, destID, typeID, historyChartRows)
		_, vol, ok := historyWindow(rows, 7)
		if !ok {
			vol = 0
		}
		dailyVolume[typeID] = vol
		return vol
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

	now := time.Now().UTC()
	var rows []tradefinderRow
	for _, o := range originStats {
		d, ok := destByType[o.TypeID]
		if !ok {
			continue
		}
		if !statFresh(o.UpdatedAt, now) || !statFresh(d.UpdatedAt, now) {
			continue // figures older than 3 days on either side: not a route
		}
		if o.TypicalBuy <= 0 || d.TypicalSell <= 0 {
			continue
		}
		margin := d.TypicalSell - o.TypicalBuy
		marginPct := margin / o.TypicalBuy * 100
		if marginPct < minMargin {
			continue
		}
		vol := dailyVolumeFor(o.TypeID)
		if vol < float64(minVolume) {
			continue
		}
		// The movable size is whichever runs out first: what
		// actually trades at the destination each day, the open
		// buy volume where the buying happens, or the open sell
		// volume where the selling happens.
		size := vol
		if float64(o.BuyVolume) < size {
			size = float64(o.BuyVolume)
		}
		if float64(d.SellVolume) < size {
			size = float64(d.SellVolume)
		}
		if size <= 0 {
			continue // any of the three at zero: this row cannot move
		}
		profit := margin * size
		if profit <= 0 {
			continue
		}
		row := tradefinderRow{
			TypeID:      o.TypeID,
			ItemName:    itemNameFor(o.TypeID),
			BuyTypical:  esi.FormatISK(o.TypicalBuy),
			SellTypical: esi.FormatISK(d.TypicalSell),
			ProfitItem:  esi.FormatISK(margin),
			MarginPct:   fmt.Sprintf("%.1f%%", marginPct),
			SoldPerDay:  esi.FormatInt(int64(math.Round(vol))),
			ProfitDay:   esi.FormatISK(profit),
			profitRaw:   profit,
		}
		if hint, ok := originCheapest[o.TypeID]; ok {
			row.OriginStation = app.scannerStationRef(ctx, hint.LocationID)
			row.OriginBest = esi.FormatISK(hint.BestSell)
		}
		if hint, ok := destBestBuy[o.TypeID]; ok {
			row.DestStation = app.scannerStationRef(ctx, hint.LocationID)
			row.DestBest = esi.FormatISK(hint.BestBuy)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].profitRaw != rows[j].profitRaw {
			return rows[i].profitRaw > rows[j].profitRaw
		}
		return rows[i].TypeID < rows[j].TypeID
	})
	if len(rows) > tradefinderRowCap {
		rows = rows[:tradefinderRowCap]
	}
	view.Rows = rows
	return view
}

// statFresh reports whether an RFC3339 stats stamp is inside the
// tradefinder's three-day freshness window. An unparseable stamp
// is not fresh: unknown-age figures never price a route.
func statFresh(rfc string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return false
	}
	return now.Sub(at) <= tradefinderMaxStatAge
}

// latestStatStamp renders the freshest stamp in a region's stats
// as "Oct 4, 3:04 PM" for the page's plain-language freshness
// lines, or "" when no stamp parses.
func latestStatStamp(stats []db.MarketRegionStat) string {
	latest := ""
	for _, s := range stats {
		if s.UpdatedAt > latest {
			latest = s.UpdatedAt
		}
	}
	at, err := time.Parse(time.RFC3339, latest)
	if err != nil {
		return ""
	}
	return at.Format("Jan 2, 3:04 PM")
}

// listStationStats reads one region's station-grain rows, logging
// and returning nil on error (hints are decoration; a failed read
// costs the hints, never the routes).
func (app *Application) listStationStats(ctx context.Context, regionID int64) []db.MarketStationStat {
	rows, err := app.queries.ListMarketStationStatsByRegion(ctx, regionID)
	if err != nil {
		log.Printf("tradefinder: list station stats for region %d: %v", regionID, err)
		return nil
	}
	return rows
}

// cheapestSellByType picks, per type, the station row holding the
// lowest sell price in a region (the "cheapest at" hint).
func cheapestSellByType(stats []db.MarketStationStat) map[int64]db.MarketStationStat {
	out := make(map[int64]db.MarketStationStat)
	for _, s := range stats {
		if s.BestSell <= 0 {
			continue
		}
		if cur, ok := out[s.TypeID]; !ok || s.BestSell < cur.BestSell {
			out[s.TypeID] = s
		}
	}
	return out
}

// bestBuyByType picks, per type, the station row holding the
// highest buy price in a region (the "best buyer at" hint).
func bestBuyByType(stats []db.MarketStationStat) map[int64]db.MarketStationStat {
	out := make(map[int64]db.MarketStationStat)
	for _, s := range stats {
		if s.BestBuy <= 0 {
			continue
		}
		if cur, ok := out[s.TypeID]; !ok || s.BestBuy > cur.BestBuy {
			out[s.TypeID] = s
		}
	}
	return out
}
