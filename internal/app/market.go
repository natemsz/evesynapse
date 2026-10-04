package app

import (
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------------
// Market: public ESI price guide + regional order books. Nothing on
// this page needs a character token — search, prices and orders are
// all unauthenticated endpoints.
// ---------------------------------------------------------------------------

// marketRegionOption is one of the fixed trade regions the page
// offers. IDs/names verified against GET /universe/regions/{id}/.
type marketRegionOption struct {
	ID   int64
	Name string
}

var marketRegions = []marketRegionOption{
	{10000002, "The Forge"},
	{10000043, "Domain"},
	{10000032, "Sinq Laison"},
	{10000030, "Heimatar"},
	{10000042, "Metropolis"},
}

const defaultMarketRegion = int64(10000002)

// maxOrderPages caps the order-book pagination so a mega-traded
// type in The Forge can't fan out into hundreds of requests; the
// page notes when the cap bites (best prices are then best-within-
// the-pages-read, not global).
const maxOrderPages = 20

// topMarketOrders is how many orders per side the tables show.
const topMarketOrders = 5

// maxSearchPrefetchWants caps the history wants a rendered
// search notes for its result types (results shown, never more).
const maxSearchPrefetchWants = 50

// suggestItem is one live-suggestion entry for the Market search
// box: the type and where its market page lives.
type suggestItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// handleMarketSuggest serves the Market search box's live
// suggestions: up to 10 marketable (market-group-bearing,
// published) SDE types whose name contains the query, prefix
// matches first. Local SDE only — no ESI, no token.
func (app *Application) handleMarketSuggest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=30")
	out := []suggestItem{}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); len(q) >= 2 {
		rows, err := app.queries.SuggestSDETypes(r.Context(), q)
		if err != nil {
			log.Printf("market: suggest %q: %v", q, err)
		}
		for _, row := range rows {
			out = append(out, suggestItem{ID: row.TypeID, Name: row.Name})
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func marketRegionName(id int64) (string, bool) {
	for _, r := range marketRegions {
		if r.ID == id {
			return r.Name, true
		}
	}
	return "", false
}

// marketMatch is one search result line (links to the item view).
type marketMatch struct {
	ID   int64
	Name string
}

// marketRegion is one entry of the region switcher on the item view.
type marketRegion struct {
	ID     int64
	Name   string
	Active bool
}

// marketOrderRow is one displayed order: display-ready strings.
type marketOrderRow struct {
	Price    string // esi.FormatISK
	Volume   string // esi.FormatInt of volume_remain
	Location string
}

// marketItem is the item view: guide prices from /markets/prices/
// plus the order book for one region. Phase 5 adds the stored
// price history (chart + changes, from market_history only — the
// worker fills it) and the watch state for "Watch this item".
type marketItem struct {
	TypeID        int64
	Name          string
	AveragePrice  string // esi.FormatISK, "" when unknown
	AdjustedPrice string // esi.FormatISK, "" when unknown
	RegionID      int64
	RegionName    string
	Regions       []marketRegion
	BestSell      string // esi.FormatISK, "" when no sell orders
	BestBuy       string // esi.FormatISK, "" when no buy orders
	BestSellLoc   string
	BestBuyLoc    string
	Spread        string // "12.3%", "" unless both sides exist
	SellOrders    int
	BuyOrders     int
	SellVolume    string // esi.FormatInt of summed volume_remain
	BuyVolume     string
	Truncated     bool // order book capped at maxOrderPages
	Sells         []marketOrderRow
	Buys          []marketOrderRow

	// Phase 5 price history (cache-only, from stored rows).
	// HistoryState is computed by attachHistory from the stored
	// rows plus the fetch-state record: historyStatePending (no
	// rows yet, fetch not settled), historyStateEmpty (worker
	// fetched, ESI had no trades), historyStateFew (a few rows —
	// summary only, no chart), or historyStateChart (enough rows
	// for the chart). The template renders a deliberate body for
	// every state; there is no blank state.
	HistoryState   string
	HistoryPending bool // true only in the pending state
	Chart          *priceChart
	Stats          *historyStats
	Change7        string // "+8.2%", "" when not computable
	Change30       string
	// HistoryLastDay is the newest recorded trade day, set when it
	// is older than historyStaleAfterDays so the section can say
	// when trading stopped instead of implying the chart is
	// current.
	HistoryLastDay string
	Watched        bool
	WatchThreshold float64
}

// watchlistRow is one watchlist line, display-ready.
type watchlistRow struct {
	TypeID    int64
	Name      string
	RegionID  int64
	Region    string
	Current   string // latest daily average, "—" when unknown
	Change7   string // "+8.2%", "—" when not computable
	Change30  string
	Moving    bool   // currently past the user's threshold
	MoveText  string // "up 8.2% over 7 days", Moving only
	Threshold string
}

// watchlistView is the /market/ watchlist section.
type watchlistView struct {
	Rows    []watchlistRow
	Query   string        // watch-search box value (wq)
	Matches []marketMatch // local candidates for the watch
}

// yourOrderRow is one of the user's open sell orders with its
// worker-computed health.
type yourOrderRow struct {
	Char     string
	CharID   int64
	Item     string
	TypeID   int64
	Price    string
	Location string
	Status   string
	Bad      bool // undercut: highlighted
}

// marketView is the Market page body.
type marketView struct {
	Query      string
	Region     int64 // active region (search results link into it)
	Matches    []marketMatch
	Item       *marketItem
	Watchlist  *watchlistView
	YourOrders []yourOrderRow
}

// handleMarket renders the Market page: a name search (local
// type-name cache plus exact ESI resolution) and, with ?type=, the
// item view for one region. Every figure is public ESI data.
func (app *Application) handleMarket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}

	q := r.URL.Query()
	view := &marketView{
		Query:  strings.TrimSpace(q.Get("q")),
		Region: defaultMarketRegion,
	}
	if rid, err := strconv.ParseInt(q.Get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(rid); ok {
			view.Region = rid
		}
	}
	data.Market = view
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))

	if typeID, err := strconv.ParseInt(q.Get("type"), 10, 64); err == nil && typeID > 0 {
		item, err := app.loadMarketItem(ctx, typeID, view.Region)
		if err != nil {
			log.Printf("market: load type %d in region %d: %v", typeID, view.Region, err)
			data.Error = "Market data unavailable right now — please try again shortly."
		} else {
			view.Item = item
		}
		app.attachHistory(ctx, view.Item, typeID, view.Region, userID)
		if view.Item != nil && view.Item.HistoryPending {
			app.notePageWant(ctx, pageWantHistory, typeID, view.Region)
		}
	} else if view.Query != "" {
		view.Matches = app.searchTypes(ctx, view.Query)
		// Prefetch: every result's history is wanted now, so a
		// tap on any of them lands on a chart instead of a wait.
		app.noteSearchHistoryWants(ctx, view.Region, view.Matches)
	}

	if userID > 0 {
		view.Watchlist = app.buildWatchlistView(ctx, userID, strings.TrimSpace(q.Get("wq")))
		view.YourOrders = app.buildYourOrders(ctx, userID)
	}

	app.render(ctx, w, http.StatusOK, "market.html", data)
}

// recentHistoryRows loads the most recent `limit` recorded trade
// rows for a (region, type) in date-ascending order. The window is
// a row count, not a calendar span: a sparse item's stored trades
// are never filtered out by an arbitrary cutoff, which used to
// leave the chart claiming it was loading for rows the worker had
// already stored.
func (app *Application) recentHistoryRows(ctx context.Context, regionID, typeID int64, limit int) []db.MarketHistory {
	rows, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: regionID, TypeID: typeID, Limit: int64(limit),
	})
	if err != nil {
		log.Printf("market: load history for type %d in region %d: %v", typeID, regionID, err)
		return nil
	}
	// The query returns newest first; chart math wants ascending.
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

// attachHistory fills an item view's history section from stored
// rows (never the network) and sets HistoryState, which the
// template turns into a deliberate body:
//
//   - pending: no rows and the worker hasn't settled the fetch —
//     the section says history is on its way, and the view leaves
//     a want for the worker.
//   - empty: the worker fetched and ESI returned no trades at
//     all — the section says so and the want settles (the 20h
//     refetch gate, keyed on the fetch-state record, is what
//     re-checks later; repeat views do not re-arm the want).
//   - few: some trades but not enough to draw — the section shows
//     the summary numbers instead of a chart.
//   - chart: enough rows — the chart plus stats; when the newest
//     row is stale, HistoryLastDay captions when trading stopped.
//
// A nil item (the live item view failed) still records the want —
// noteSearchHistoryWants records history wants for every type a
// search just rendered, so whichever result the user taps has its
// fetch already queued at top priority. Bounded by the number of
// matches shown (capped at maxSearchPrefetchWants); the handler
// only writes rows — the urgent drain and the cycle do the
// fetching, never this.
func (app *Application) noteSearchHistoryWants(ctx context.Context, regionID int64, matches []marketMatch) {
	stamp := time.Now().UTC().Format(time.RFC3339)
	noted := 0
	for _, m := range matches {
		if noted >= maxSearchPrefetchWants {
			break
		}
		if m.ID <= 0 {
			continue
		}
		if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: m.ID, LastRequestedAt: stamp,
		}); err != nil {
			log.Printf("market: prefetch want for type %d in region %d: %v", m.ID, regionID, err)
			continue
		}
		app.notePageWant(ctx, pageWantHistory, m.ID, regionID)
		noted++
	}
}

// the user asked about this type either way.
func (app *Application) attachHistory(ctx context.Context, item *marketItem, typeID, regionID, userID int64) {
	rows := app.recentHistoryRows(ctx, regionID, typeID, historyChartRows)
	if len(rows) == 0 {
		if item != nil {
			item.HistoryState = historyStatePending
			item.HistoryPending = true
		}
		if app.historyFetchSettled(ctx, regionID, typeID) {
			if item != nil {
				item.HistoryState = historyStateEmpty
				item.HistoryPending = false
			}
			return
		}
		if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: typeID,
			LastRequestedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			log.Printf("market: record history want for type %d in region %d: %v", typeID, regionID, err)
		}
		return
	}
	if item == nil {
		return
	}
	if len(rows) == 1 {
		item.HistoryState = historyStateFew
	} else {
		item.HistoryState = historyStateChart
		if chart, ok := buildPriceChart(rows); ok {
			c := chart
			item.Chart = &c
		}
	}
	if stats, ok := summarizeHistory(rows); ok {
		item.Stats = &stats
	}
	if newest, err := time.Parse(historyDateLayout, rows[len(rows)-1].Date); err == nil &&
		time.Since(newest) > historyStaleAfterDays*24*time.Hour {
		item.HistoryLastDay = rows[len(rows)-1].Date
	}
	if pct, ok := historyChangePct(rows, 7); ok {
		item.Change7 = formatChangePct(pct)
	}
	if pct, ok := historyChangePct(rows, 30); ok {
		item.Change30 = formatChangePct(pct)
	}
	if userID > 0 {
		if entry, err := app.queries.GetWatchlistEntry(ctx, db.GetWatchlistEntryParams{
			UserID: userID, TypeID: typeID, RegionID: regionID,
		}); err == nil {
			item.Watched = true
			item.WatchThreshold = entry.ThresholdPct
		}
	}
}

// historyFetchSettled reports whether the worker has ever
// completed a history fetch for this (region, type) — the marker
// that distinguishes "ESI has no trades for this" (empty state)
// from "never asked" (pending state).
func (app *Application) historyFetchSettled(ctx context.Context, regionID, typeID int64) bool {
	state, err := app.queries.GetMarketFetchState(ctx, marketFetchKind("history", marketKey{RegionID: regionID, TypeID: typeID}))
	return err == nil && state.State == fetchStateOK
}

// searchTypes merges exact /universe/ids/ hits (first) with partial
// matches from the local SDE type table. Exact hits are persisted
// to type_names so the fallback cache finds them too.
func (app *Application) searchTypes(ctx context.Context, query string) []marketMatch {
	var matches []marketMatch
	seen := make(map[int64]bool)

	var ids esi.UniverseIDs
	if err := app.esi.PostJSON(ctx, "/universe/ids/", []string{query}, &ids); err != nil {
		log.Printf("market: exact lookup for %q: %v", query, err)
	} else {
		for _, hit := range ids.InventoryTypes {
			if hit.ID <= 0 || hit.Name == "" || seen[hit.ID] {
				continue
			}
			seen[hit.ID] = true
			matches = append(matches, marketMatch{ID: hit.ID, Name: hit.Name})
			if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: hit.ID, Name: hit.Name}); err != nil {
				log.Printf("market: persist type name %d: %v", hit.ID, err)
			}
		}
	}

	rows, err := app.queries.SearchSDETypes(ctx, "%"+query+"%")
	if err != nil {
		log.Printf("market: local search for %q: %v", query, err)
		return matches
	}
	for _, row := range rows {
		if seen[row.TypeID] {
			continue
		}
		seen[row.TypeID] = true
		matches = append(matches, marketMatch{ID: row.TypeID, Name: row.Name})
	}
	return matches
}

// marketPrices returns the full /markets/prices/ guide keyed by type
// ID, cached in memory until the response's Expires header says
// otherwise (6h fallback). A failed refresh serves the stale cache
// when one exists.
func (app *Application) marketPrices(ctx context.Context) (map[int64]esi.MarketPrice, error) {
	app.pricesMu.Lock()
	defer app.pricesMu.Unlock()

	if len(app.prices) > 0 && time.Now().Before(app.pricesExpiry) {
		return app.prices, nil
	}

	body, header, err := app.esi.FetchRaw(ctx, "", "/markets/prices/")
	if err != nil {
		if len(app.prices) > 0 {
			log.Printf("market: refresh prices failed (%v); serving stale cache", err)
			return app.prices, nil
		}
		return nil, err
	}
	var rows []esi.MarketPrice
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("market: decode prices: %w", err)
	}

	prices := make(map[int64]esi.MarketPrice, len(rows))
	for _, row := range rows {
		prices[row.TypeID] = row
	}
	expiry := time.Now().Add(6 * time.Hour)
	if exp := header.Get("Expires"); exp != "" {
		if t, perr := http.ParseTime(exp); perr == nil {
			expiry = t
		}
	}
	app.prices = prices
	app.pricesExpiry = expiry
	return prices, nil
}

// cachedPrices returns the in-memory /markets/prices/ cache when a
// Market visit has populated it, nil otherwise. It never fetches:
// render-path consumers (killmail values) show "—" until the cache
// exists rather than blocking a page on the price guide.
func (app *Application) cachedPrices() map[int64]esi.MarketPrice {
	app.pricesMu.Lock()
	defer app.pricesMu.Unlock()
	if len(app.prices) == 0 {
		return nil
	}
	return app.prices
}

// fetchOrderBook reads one region's orders for a type, following
// X-Pages up to maxOrderPages. Split into sides, unsorted.
func (app *Application) fetchOrderBook(ctx context.Context, regionID, typeID int64) (sells, buys []esi.MarketOrder, truncated bool, err error) {
	path := fmt.Sprintf("/markets/%d/orders/?type_id=%d&order_type=all", regionID, typeID)

	var all []esi.MarketOrder
	totalPages := 1
	for page := 1; page <= totalPages && page <= maxOrderPages; page++ {
		body, header, ferr := app.esi.FetchRaw(ctx, "", fmt.Sprintf("%s&page=%d", path, page))
		if ferr != nil {
			return nil, nil, false, ferr
		}
		if page == 1 {
			if xp := header.Get("X-Pages"); xp != "" {
				if n, aerr := strconv.Atoi(xp); aerr == nil && n > 1 {
					totalPages = n
				}
			}
			truncated = totalPages > maxOrderPages
		}
		var orders []esi.MarketOrder
		if derr := json.Unmarshal(body, &orders); derr != nil {
			return nil, nil, false, fmt.Errorf("market: decode orders page %d: %w", page, derr)
		}
		all = append(all, orders...)
	}

	for _, o := range all {
		if o.IsBuyOrder {
			buys = append(buys, o)
		} else {
			sells = append(sells, o)
		}
	}
	return sells, buys, truncated, nil
}

// loadMarketItem builds the item view for (type, region): guide
// prices plus the best and top orders of the regional book.
func (app *Application) loadMarketItem(ctx context.Context, typeID, regionID int64) (*marketItem, error) {
	item := &marketItem{
		TypeID:   typeID,
		Name:     app.esi.TypeName(ctx, typeID),
		RegionID: regionID,
	}
	item.RegionName, _ = marketRegionName(regionID)
	for _, reg := range marketRegions {
		item.Regions = append(item.Regions, marketRegion{
			ID:     reg.ID,
			Name:   reg.Name,
			Active: reg.ID == regionID,
		})
	}

	prices, err := app.marketPrices(ctx)
	if err != nil {
		return nil, err
	}
	if p, ok := prices[typeID]; ok {
		if p.AveragePrice > 0 {
			item.AveragePrice = esi.FormatISK(p.AveragePrice)
		}
		if p.AdjustedPrice > 0 {
			item.AdjustedPrice = esi.FormatISK(p.AdjustedPrice)
		}
	}

	sells, buys, truncated, err := app.fetchOrderBook(ctx, regionID, typeID)
	if err != nil {
		return nil, err
	}
	item.Truncated = truncated
	item.SellOrders = len(sells)
	item.BuyOrders = len(buys)

	var sellVol, buyVol int64
	for _, o := range sells {
		sellVol += o.VolumeRemain
	}
	for _, o := range buys {
		buyVol += o.VolumeRemain
	}
	item.SellVolume = esi.FormatInt(sellVol)
	item.BuyVolume = esi.FormatInt(buyVol)

	sort.Slice(sells, func(i, j int) bool { return sells[i].Price < sells[j].Price })
	sort.Slice(buys, func(i, j int) bool { return buys[i].Price > buys[j].Price })

	// Locations the book shows join the structure-name queue so
	// the worker resolves their names in the background; this
	// render only ever reads the cache (a queue note, same as the
	// history want an item view leaves).
	{
		ids := make([]int64, 0, len(sells)+len(buys))
		for _, o := range sells {
			ids = append(ids, o.LocationID)
		}
		for _, o := range buys {
			ids = append(ids, o.LocationID)
		}
		app.noteStructureIDs(ctx, ids...)
	}

	if len(sells) > 0 {
		item.BestSell = esi.FormatISK(sells[0].Price)
		item.BestSellLoc = app.orderLocation(ctx, sells[0].LocationID, sells[0].SystemID)
	}
	if len(buys) > 0 {
		item.BestBuy = esi.FormatISK(buys[0].Price)
		item.BestBuyLoc = app.orderLocation(ctx, buys[0].LocationID, buys[0].SystemID)
	}
	if len(sells) > 0 && len(buys) > 0 && buys[0].Price > 0 {
		item.Spread = fmt.Sprintf("%.1f%%", (sells[0].Price-buys[0].Price)/buys[0].Price*100)
	}

	for i, o := range sells {
		if i >= topMarketOrders {
			break
		}
		item.Sells = append(item.Sells, marketOrderRow{
			Price:    esi.FormatISK(o.Price) + " ISK",
			Volume:   esi.FormatInt(o.VolumeRemain),
			Location: app.orderLocation(ctx, o.LocationID, o.SystemID),
		})
	}
	for i, o := range buys {
		if i >= topMarketOrders {
			break
		}
		item.Buys = append(item.Buys, marketOrderRow{
			Price:    esi.FormatISK(o.Price) + " ISK",
			Volume:   esi.FormatInt(o.VolumeRemain),
			Location: app.orderLocation(ctx, o.LocationID, o.SystemID),
		})
	}
	return item, nil
}

// orderLocation renders where an order sits: the NPC station name
// when the location is one, the solar-system name for system-level
// orders (and as fallback), and for player structures the name
// the worker has resolved (structures.go) or an honest
// "Structure #<id>" until it lands. Station/system names reuse
// the shared place-name cache. Cache-only: never fetches.
func (app *Application) orderLocation(ctx context.Context, locationID, systemID int64) string {
	// Upwell structure IDs live up around 1e12, far above the
	// station (6e7) and system (3e7) ranges.
	if locationID > 1_000_000_000 {
		if name := app.resolvedStructureTitle(ctx, locationID); name != "" {
			return name
		}
		return fmt.Sprintf("Structure #%d", locationID)
	}
	if locationID != 0 && locationID != systemID {
		if name := app.esi.PlaceName(ctx, fmt.Sprintf("/universe/stations/%d/", locationID), locationID, ""); name != "" {
			return name
		}
	}
	return app.esi.PlaceName(ctx, fmt.Sprintf("/universe/systems/%d/", systemID), systemID, fmt.Sprintf("System #%d", systemID))
}

// marketRegionLabel names a region for display: the five trade
// hubs by their short names, anything else from the SDE region
// table, and an honest placeholder for ids neither knows.
func (app *Application) marketRegionLabel(ctx context.Context, regionID int64) string {
	if name, ok := marketRegionName(regionID); ok {
		return name
	}
	if row, err := app.queries.GetSDERegion(ctx, regionID); err == nil && row.Name != "" {
		return row.Name
	}
	return fmt.Sprintf("Region #%d", regionID)
}

// buildWatchlistView assembles the watchlist section: the user's
// watched types with their current average and 7/30-day moves,
// plus local search matches when the watch search box was used.
// Stored rows only.
func (app *Application) buildWatchlistView(ctx context.Context, userID int64, watchQuery string) *watchlistView {
	view := &watchlistView{Query: watchQuery}
	if len(watchQuery) >= 2 {
		if rows, err := app.queries.SuggestSDETypes(ctx, watchQuery); err != nil {
			log.Printf("market: watch search %q: %v", watchQuery, err)
		} else {
			for _, row := range rows {
				view.Matches = append(view.Matches, marketMatch{ID: row.TypeID, Name: row.Name})
			}
			// Prefetch, same as the main search: a tap on any
			// match should land on a chart.
			app.noteSearchHistoryWants(ctx, defaultMarketRegion, view.Matches)
		}
	}
	entries, err := app.queries.ListWatchlistByUser(ctx, userID)
	if err != nil {
		log.Printf("market: list watchlist for user %d: %v", userID, err)
		return view
	}
	for _, e := range entries {
		row := watchlistRow{
			TypeID:    e.TypeID,
			Name:      app.typeNameOrID(ctx, e.TypeID),
			RegionID:  e.RegionID,
			Region:    app.marketRegionLabel(ctx, e.RegionID),
			Current:   "—",
			Change7:   "—",
			Change30:  "—",
			Threshold: fmt.Sprintf("%g", e.ThresholdPct),
		}
		rows := app.recentHistoryRows(ctx, e.RegionID, e.TypeID, historyChartRows)
		if len(rows) > 0 {
			row.Current = esi.FormatISK(rows[len(rows)-1].Average)
		}
		if pct, ok := historyChangePct(rows, 7); ok {
			row.Change7 = formatChangePct(pct)
			if math.Abs(pct) >= e.ThresholdPct {
				row.Moving = true
				row.MoveText = changeDirection(pct) + " over 7 days"
			}
		}
		if pct, ok := historyChangePct(rows, 30); ok {
			row.Change30 = formatChangePct(pct)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// buildYourOrders assembles the "Your orders" section: every open
// sell order across the user's characters with the health verdict
// the worker computed (or a "not checked yet" line before the
// first book pass lands).
func (app *Application) buildYourOrders(ctx context.Context, userID int64) []yourOrderRow {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("market: your orders: list characters for user %d: %v", userID, err)
		return nil
	}
	health := make(map[int64]db.OrderHealth)
	if rows, err := app.queries.ListOrderHealthByUser(ctx, userID); err != nil {
		log.Printf("market: your orders: list health for user %d: %v", userID, err)
	} else {
		for _, h := range rows {
			health[h.OrderID] = h
		}
	}
	var out []yourOrderRow
	for _, ch := range chars {
		orders, ok := app.loadOrdersSnapshot(ctx, ch.CharacterID)
		if !ok {
			continue
		}
		sort.SliceStable(orders, func(i, j int) bool {
			if orders[i].TypeID != orders[j].TypeID {
				return orders[i].TypeID < orders[j].TypeID
			}
			return orders[i].OrderID < orders[j].OrderID
		})
		for _, o := range orders {
			if o.IsBuyOrder || o.VolumeRemain <= 0 {
				continue
			}
			row := yourOrderRow{
				Char:     ch.Name,
				CharID:   ch.CharacterID,
				Item:     app.typeNameOrID(ctx, o.TypeID),
				TypeID:   o.TypeID,
				Price:    esi.FormatISK(o.Price) + " ISK",
				Location: app.econLocationTitle(ctx, o.LocationID),
				Status:   "Not checked against the order book yet",
			}
			if h, ok := health[o.OrderID]; ok && h.CharacterID == ch.CharacterID {
				row.Status, row.Bad = orderHealthText(h.MyPrice, h.Status, h.StationBest, h.RegionBest)
			}
			out = append(out, row)
		}
	}
	return out
}

// orderHealthText turns a stored verdict into the user's line.
// Prices are display-ready; percents are against the order's own
// price.
func orderHealthText(myPrice float64, status string, stationBest, regionBest float64) (string, bool) {
	undercutBy := func(best float64) string {
		if best <= 0 || myPrice <= 0 {
			return ""
		}
		return fmt.Sprintf(" by %s ISK (%.1f%%)", esi.FormatISK(myPrice-best), (myPrice-best)/myPrice*100)
	}
	switch status {
	case "undercut_station":
		return "Undercut" + undercutBy(stationBest) + " at this station", true
	case "undercut_region":
		return "Undercut" + undercutBy(regionBest) + " elsewhere in the region", true
	case "best_region_cheaper":
		text := "Best price here"
		if regionBest > 0 {
			text += fmt.Sprintf(" — cheaper in the region (best %s ISK)", esi.FormatISK(regionBest))
		}
		return text, false
	default: // "best"
		return "Best price here", false
	}
}

// handleMarketWatch applies one watchlist change (POST
// /market/watch): add (from an item page or the watch search),
// threshold update, or removal. Thresholds clamp to 1–50 — the
// number users type is a percent, and a 0 would alert on every
// flicker while 500 would never fire.
func (app *Application) handleMarketWatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	next := "/market/"
	if err := r.ParseForm(); err == nil {
		if n := r.Form.Get("next"); strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") {
			next = n
		}
		if userID > 0 {
			typeID, _ := strconv.ParseInt(r.Form.Get("type"), 10, 64)
			regionID, _ := strconv.ParseInt(r.Form.Get("region"), 10, 64)
			if regionID <= 0 {
				regionID = defaultMarketRegion
			}
			switch r.Form.Get("action") {
			case "add", "update":
				threshold := 5.0
				if v, err := strconv.ParseFloat(r.Form.Get("threshold"), 64); err == nil && !math.IsNaN(v) {
					threshold = v
				}
				if threshold < 1 {
					threshold = 1
				}
				if threshold > 50 {
					threshold = 50
				}
				if typeID > 0 {
					if err := app.queries.UpsertWatchlistEntry(ctx, db.UpsertWatchlistEntryParams{
						UserID: userID, TypeID: typeID, RegionID: regionID,
						ThresholdPct: threshold,
						CreatedAt:    time.Now().UTC().Format(time.RFC3339),
					}); err != nil {
						log.Printf("market: watch upsert type %d for user %d: %v", typeID, userID, err)
					}
				}
			case "remove":
				if typeID > 0 {
					if err := app.queries.DeleteWatchlistEntry(ctx, db.DeleteWatchlistEntryParams{
						UserID: userID, TypeID: typeID, RegionID: regionID,
					}); err != nil {
						log.Printf("market: watch remove type %d for user %d: %v", typeID, userID, err)
					}
				}
			}
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
