package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/markethistory"
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

// topMarketOrders is how many orders per side the tables show.
const topMarketOrders = 5

// maxSearchPrefetchWants caps the history wants a rendered
// search notes for its result types (results shown, never more).
const maxSearchPrefetchWants = 50

// globalMarketRegion is ESI's region for EVE's one global market, where
// PLEX has traded since 2021 instead of in any regional market.
const globalMarketRegion = int64(19000001)

// globalMarketTypes trade only on the global market: PLEX.
var globalMarketTypes = map[int64]bool{44992: true}

// marketRegionForType is the region a type's market lives in: the
// global market for a global-market type, whatever was asked for;
// otherwise the region asked for.
func marketRegionForType(typeID, regionID int64) int64 {
	if globalMarketTypes[typeID] {
		return globalMarketRegion
	}
	return regionID
}

func marketRegionName(id int64) (string, bool) {
	if id == globalMarketRegion {
		return "Global market", true
	}
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
	Price  string // esi.FormatISK
	Volume string // esi.FormatInt of volume_remain
	Loc    placeRef
}

// marketItem is the item view: guide prices from /markets/prices/
// plus the order book for one region, the stored
// price history (chart + changes, from market_history only — the
// worker fills it) and the watch state for "Watch this item".
type marketItem struct {
	TypeID int64
	Name   string
	// Anonymous: rendered for a visitor without a session. The page
	// is read-only for them: no watch controls, and the order book
	// comes from what is held -- a freshly fetched book, or the
	// stored sweep snapshot (BookStoredAt set) -- never a fetch of
	// their own (BookPending when neither exists, or when the
	// anonymous-market switch is off).
	Anonymous     bool
	BookPending   bool
	BookStoredAt  string // age of the stored sweep book shown, "" unless it came from one
	AveragePrice  string // esi.FormatISK, "" when unknown
	AdjustedPrice string // esi.FormatISK, "" when unknown
	// The worker's estimated sell (truevalue.go) for a type
	// with no guide average: the Adjusted price card then
	// presents this as the "Estimated value" instead, with
	// EstimatedSellNote as its basis line.
	EstimatedSell     string // esi.FormatISK, "" when no true value applies
	EstimatedSellNote string
	RegionID          int64
	RegionName        string
	Regions           []marketRegion
	BestSell          string // esi.FormatISK, "" when no sell orders
	BestBuy           string // esi.FormatISK, "" when no buy orders
	BestSellLoc       placeRef
	BestBuyLoc        placeRef
	Spread            string // "12.3%", "" unless both sides exist and the book is deep enough
	SpreadBand        string // color band for the spread card: white|pastel|green|blue
	SpreadThin        string // thin-book note for the spread card when a side is too shallow to quote, "" otherwise
	SellOrders        int
	BuyOrders         int
	SellVolume        string // esi.FormatInt of summed volume_remain
	BuyVolume         string
	Truncated         bool // order book capped at maxOrderPages
	Sells             []marketOrderRow
	Buys              []marketOrderRow

	BestSellRaw float64 // numeric bests behind the formatted strings (0 = none)
	BestBuyRaw  float64

	// Honest prices: stats over the whole book behind the bests,
	// so one joke order cannot stand in for the market. Typical*
	// is the median order; the band lines give the price 9 in 10
	// orders are at-or-under (sells) / at-or-over (buys).
	TypicalSell string // esi.FormatISK, "" when no sell orders
	SellBand    string
	TypicalBuy  string
	BuyBand     string
	Trader      *markethistory.TraderStats // trading snapshot; set by attachHistory

	// Price history (cache-only, from stored rows).
	// HistoryState is computed by attachHistory from the stored
	// rows plus the fetch-state record: markethistory.StatePending (no
	// rows yet, fetch not settled), markethistory.StateEmpty (worker
	// fetched, ESI had no trades), markethistory.StateFew (a few rows —
	// summary only, no chart), or markethistory.StateChart (enough rows
	// for the chart). The template renders a deliberate body for
	// every state; there is no blank state.
	// AvgSparkPoints/AvgSparkArea draw the Average price card's
	// sparkline from the last 30 stored history rows (set by
	// attachHistory); empty until two or more days are stored.
	AvgSparkPoints string
	AvgSparkArea   string

	HistoryState   string
	HistoryPending bool // true only in the pending state
	Chart          *markethistory.PriceChart
	Stats          *markethistory.Stats
	Change7        string // "+8.2%", "" when not computable
	Change30       string
	// HistoryLastDay is the newest recorded trade day, set when it
	// is older than markethistory.StaleAfterDays so the section can say
	// when trading stopped instead of implying the chart is
	// current.
	HistoryLastDay string
	Watched        bool
	WatchThreshold float64

	// RegionStats is the hub-regions strip (P1): one row per hub
	// region from the worker's stored sweep stats, filled by
	// attachRegionStats straight from the database — rendering
	// a region never triggers a fetch. Empty until the item view
	// is built with a type.
	RegionStats []marketRegionStatRow
	// AllRegions: the strip lists every region a stored row speaks
	// for (?regions=all), not just the hubs.
	AllRegions bool
	// Pack is the region's stat pack section (market_trend_view.go).
	Pack *statPackView
}

// spreadBookDepth is the deepest thin book, per side, whose
// spread the item page still refuses to quote: at this many
// orders or fewer, a side's median is one order's price, so
// the spread card names the thin book instead (see loadMarketItem).
const spreadBookDepth = 3

// thinBookNote words the spread card's thin-book state: which
// side is too shallow to quote a spread from, and by how much.
func thinBookNote(sellOrders, buyOrders int) string {
	orders := func(n int, side string) string {
		if n == 1 {
			return "only 1 " + side + " order"
		}
		return fmt.Sprintf("only %d %s orders", n, side)
	}
	switch {
	case sellOrders <= spreadBookDepth && buyOrders <= spreadBookDepth:
		return "Too few orders to quote a spread: " +
			orders(sellOrders, "sell") + " and " + orders(buyOrders, "buy") + "."
	case sellOrders <= spreadBookDepth:
		return "Too few orders to quote a spread: " + orders(sellOrders, "sell") + "."
	default:
		return "Too few orders to quote a spread: " + orders(buyOrders, "buy") + "."
	}
}

// spreadBand names the spread card's color band for a spread
// percentage, banded on the value as displayed (one decimal):
// a crossed book (negative spread) reads red, at or under 1%
// white, through 3% a soft pastel green, through 5% a fuller
// green, beyond that a legible light baby blue.
func spreadBand(pct float64) string {
	switch rounded := math.Round(pct*10) / 10; {
	case rounded < 0:
		return "red"
	case rounded <= 1:
		return "white"
	case rounded <= 3:
		return "pastel"
	case rounded <= 5:
		return "green"
	default:
		return "blue"
	}
}

// TraderPollURL is the trading snapshot's live-region target:
// the region/type plus the order book's best prices, which the
// page itself already fetched live. The fragment handler stays
// cache-only — the bests ride along from the render that had
// them, so the margin survives the live-fill swap unchanged
// instead of flickering back to a dash.
func (item *marketItem) TraderPollURL() string {
	return fmt.Sprintf("/market/trader-fragment?region=%d&type=%d&bs=%s&bb=%s",
		item.RegionID, item.TypeID,
		strconv.FormatFloat(item.BestSellRaw, 'f', -1, 64),
		strconv.FormatFloat(item.BestBuyRaw, 'f', -1, 64))
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
	Char   string
	CharID int64
	Item   string
	TypeID int64
	Price  string
	Loc    placeRef
	Status string
	Bad    bool // undercut: highlighted
}

// marketView is the Market page body.
type marketView struct {
	Query      string
	Region     int64 // active region (search results link into it)
	Matches    []marketMatch
	Item       *marketItem
	Watchlist  *watchlistView
	YourOrders []yourOrderRow
	Browse     *marketBrowseView
}

// marketBrowseGroup is one node of the category browser: a
// market group with its ember glyph.
type marketBrowseGroup struct {
	ID       int64
	Name     string
	Icon     template.HTML
	IconKey  string
	HasTypes bool
}

// handleMarket renders the Market page: a name search (local
// type-name cache plus exact ESI resolution) and, with ?type=, the
// item view for one region. Every figure is public ESI data.
func (app *Application) handleMarket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: anonymous visitors read stored data only

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
	userID := app.userID(ctx)
	// Anonymous renders never fetch and never queue anything but
	// market wants: search reads the local SDE only, no guesses,
	// but market history wants are theirs to leave too (the public
	// market endpoints carry a huge ESI allowance, and the Market
	// page is a public-facing tool) -- unless the anonymous-market
	// switch is off (market_anon.go), which restores the old rule
	// that a signed-in visit warms what an anonymous one then reads.
	anon := userID == 0
	anonMarket := anon && !app.anonMarketBlocked()

	if typeID, err := strconv.ParseInt(q.Get("type"), 10, 64); err == nil && typeID > 0 {
		view.Region = marketRegionForType(typeID, view.Region)
		if !anon {
			app.foresightOpened(ctx, userID, foresightMarketView(typeID))
		}
		item, err := app.loadMarketItem(ctx, typeID, view.Region, anon)
		if err != nil {
			logging.Errorf("market: load type %d in region %d: %v", typeID, view.Region, err)
			data.Error = "Market data unavailable right now — please try again shortly."
		} else {
			view.Item = item
		}
		if view.Item != nil || !anon {
			app.attachHistory(ctx, view.Item, typeID, view.Region, userID)
		}
		if view.Item != nil && q.Get("regions") == "all" {
			view.Item.AllRegions = true
			app.attachAllRegionStats(ctx, view.Item)
		} else {
			app.attachRegionStats(ctx, view.Item)
		}
		app.attachStatPack(ctx, view.Item)
		app.attachMarketChanges(ctx, view.Item)
		if !anon {
			app.markGuessHit(ctx, pageWantHistory, typeID, view.Region)
		}
		if (!anon || anonMarket) && view.Item != nil && view.Item.HistoryPending {
			app.notePageWant(ctx, pageWantHistory, typeID, view.Region)
		}
	} else if view.Query != "" {
		view.Matches = app.searchTypes(ctx, view.Query, anon)
		// Prefetch: every result's history is wanted now, so a
		// tap on any of them lands on a chart instead of a wait.
		// Anonymous searches prefetch too while the market switch
		// is on: these are market wants like any other.
		if !anon || anonMarket {
			app.noteSearchHistoryWants(ctx, view.Region, view.Matches)
		}
	}

	// The category browser rides every non-item render: the
	// top-level groups on the landing, or the requested group's
	// children, breadcrumbs and leaf types when ?group= is in
	// play. Pure local SDE reads either way.
	if view.Item == nil {
		view.Browse = app.buildMarketBrowse(ctx, view.Region, q)
	}

	if userID > 0 {
		view.Watchlist = app.buildWatchlistView(ctx, userID, strings.TrimSpace(q.Get("wq")))
		view.YourOrders = app.buildYourOrders(ctx, userID)
	}

	app.render(ctx, w, http.StatusOK, "market.html", data)
}

// marketBrowseGroupRow adapts one sqlc market-group row to its
// display row, glyph included.
func marketBrowseGroupRow(row db.SdeMarketGroup) marketBrowseGroup {
	return marketBrowseGroup{
		ID:       row.MarketGroupID,
		Name:     row.Name,
		Icon:     marketGroupIconSVG(row.Name, row.MarketGroupID),
		IconKey:  marketGroupIconKey(row.Name, row.MarketGroupID),
		HasTypes: row.HasTypes > 0,
	}
}

// recentHistoryRows loads the most recent `limit` recorded trade rows
// for a (region, type) in date-ascending order. The window is a row
// count, not a calendar span: a sparse item's stored trades are never
// filtered out by an arbitrary cutoff.
func (app *Application) recentHistoryRows(ctx context.Context, regionID, typeID int64, limit int) []db.MarketHistory {
	rows, err := app.queries.ListMarketHistory(ctx, db.ListMarketHistoryParams{
		RegionID: regionID, TypeID: typeID, RowLimit: int64(limit),
	})
	if err != nil {
		logging.Errorf("market: load history for type %d in region %d: %v", typeID, regionID, err)
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
	now := time.Now().UTC()
	noted := 0
	for _, m := range matches {
		if noted >= maxSearchPrefetchWants {
			break
		}
		if m.ID <= 0 {
			continue
		}
		if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: m.ID, LastRequestedAt: now, Priority: viewedPriority(ctx),
		}); err != nil {
			logging.Errorf("market: prefetch want for type %d in region %d: %v", m.ID, regionID, err)
			continue
		}
		app.notePageWant(ctx, pageWantHistory, m.ID, regionID)
		noted++
	}
}

// the user asked about this type either way.
func (app *Application) attachHistory(ctx context.Context, item *marketItem, typeID, regionID, userID int64) {
	rows := app.recentHistoryRows(ctx, regionID, typeID, markethistory.ChartRows)
	if item != nil {
		// The trading snapshot rides on whatever rows exist
		// (possibly none yet) plus the book's bests; missing
		// figures render as dashes, never invented numbers.
		item.Trader = markethistory.BuildTraderStats(rows, item.BestSellRaw, item.BestBuyRaw)
	}
	if len(rows) == 0 {
		if item != nil {
			item.HistoryState = markethistory.StatePending
			item.HistoryPending = true
		}
		if app.historyFetchSettled(ctx, regionID, typeID) {
			if item != nil {
				item.HistoryState = markethistory.StateEmpty
				item.HistoryPending = false
			}
			return
		}
		if item != nil && item.Anonymous && app.anonMarketBlocked() {
			return // the market switch is off: anonymous renders queue no work
		}
		if err := app.queries.UpsertMarketHistoryWant(ctx, db.UpsertMarketHistoryWantParams{
			RegionID: regionID, TypeID: typeID,
			LastRequestedAt: time.Now().UTC(), Priority: viewedPriority(ctx),
		}); err != nil {
			logging.Errorf("market: record history want for type %d in region %d: %v", typeID, regionID, err)
		}
		return
	}
	if item == nil {
		return
	}
	// The Average price card's sparkline: the last 30 stored
	// days, from the same rows the chart below is drawn from.
	sparkRows := rows
	if len(sparkRows) > 30 {
		sparkRows = sparkRows[len(sparkRows)-30:]
	}
	if points, area, ok := markethistory.Sparkline(sparkRows); ok {
		item.AvgSparkPoints = points
		item.AvgSparkArea = area
	}
	if len(rows) == 1 {
		item.HistoryState = markethistory.StateFew
	} else {
		item.HistoryState = markethistory.StateChart
		if chart, ok := markethistory.BuildPriceChart(rows); ok {
			c := chart
			item.Chart = &c
		}
	}
	if stats, ok := markethistory.Summarize(rows); ok {
		item.Stats = &stats
	}
	if newest, err := time.Parse(markethistory.DateLayout, rows[len(rows)-1].Date); err == nil &&
		time.Since(newest) > markethistory.StaleAfterDays*24*time.Hour {
		item.HistoryLastDay = rows[len(rows)-1].Date
	}
	if pct, ok := markethistory.ChangePct(rows, 7); ok {
		item.Change7 = markethistory.FormatChangePct(pct)
	}
	if pct, ok := markethistory.ChangePct(rows, 30); ok {
		item.Change30 = markethistory.FormatChangePct(pct)
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

// exactTypeLookupRetry is how long a background exact-name
// resolution rests before a later search may set it going again:
// a name already under resolution is not asked twice, and one
// ESI settled as no type (or could not settle) is not re-asked
// on every search.
const exactTypeLookupRetry = 10 * time.Minute

// searchTypes merges exact hits (first) with partial matches
// from the local SDE type table. The exact hits are read from
// the stored type_names cache; a name the cache cannot settle
// is resolved from ESI in the background and stored for the
// next search, so the request thread never waits on the
// lookup. Anonymous searches are the local SDE only, exactly
// as before: no cache read, no lookup noted.
func (app *Application) searchTypes(ctx context.Context, query string, localOnly bool) []marketMatch {
	var matches []marketMatch
	seen := make(map[int64]bool)

	if !localOnly {
		stored, err := app.queries.ListTypeNamesByExactName(ctx, query)
		if err != nil {
			logging.Errorf("market: stored exact lookup for %q: %v", query, err)
		}
		for _, hit := range stored {
			if hit.TypeID <= 0 || hit.Name == "" || seen[hit.TypeID] {
				continue
			}
			seen[hit.TypeID] = true
			matches = append(matches, marketMatch{ID: hit.TypeID, Name: hit.Name})
		}
		if len(matches) == 0 {
			app.noteExactTypeLookup(query)
		}
	}

	rows, err := app.queries.SearchSDETypes(ctx, "%"+query+"%")
	if err != nil {
		logging.Errorf("market: local search for %q: %v", query, err)
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

// noteExactTypeLookup sets a background resolution of one exact
// type name going, unless one is already under way or rested
// recently. Writes a memo entry only; the resolution itself runs
// off the request thread.
func (app *Application) noteExactTypeLookup(query string) {
	key := strings.ToLower(strings.Join(strings.Fields(query), " "))
	if key == "" {
		return
	}
	now := time.Now()
	app.typeLookupMu.Lock()
	if app.typeLookupTried == nil {
		app.typeLookupTried = make(map[string]time.Time)
	}
	if len(app.typeLookupTried) >= 2000 {
		for k, tried := range app.typeLookupTried {
			if now.Sub(tried) >= exactTypeLookupRetry {
				delete(app.typeLookupTried, k)
			}
		}
	}
	if tried, ok := app.typeLookupTried[key]; ok && now.Sub(tried) < exactTypeLookupRetry {
		app.typeLookupMu.Unlock()
		return
	}
	app.typeLookupTried[key] = now
	app.typeLookupMu.Unlock()

	ctx := app.workerCtx
	if ctx == nil {
		ctx = context.Background()
	}
	go app.resolveExactTypeName(ctx, query)
}

// resolveExactTypeName asks ESI to settle one exact type name
// and persists the hits to type_names, where later searches
// read them.
func (app *Application) resolveExactTypeName(ctx context.Context, query string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var ids esi.UniverseIDs
	if err := app.esi.PostJSON(ctx, "/universe/ids/", []string{query}, &ids); err != nil {
		logging.Errorf("market: exact lookup for %q: %v", query, err)
		return
	}
	for _, hit := range ids.InventoryTypes {
		if hit.ID <= 0 || hit.Name == "" {
			continue
		}
		if err := app.queries.UpsertTypeName(ctx, db.UpsertTypeNameParams{TypeID: hit.ID, Name: hit.Name}); err != nil {
			logging.Errorf("market: persist type name %d: %v", hit.ID, err)
		}
	}
}

// marketPrices returns the full /markets/prices/ guide keyed by type
// ID, cached in memory until the response's Expires header says
// otherwise (6h fallback). A failed refresh serves the stale cache
// when one exists.
func (app *Application) marketPrices(ctx context.Context) (map[int64]esi.MarketPrice, error) {
	app.pricesMu.Lock()
	if len(app.prices) > 0 && time.Now().Before(app.pricesExpiry) {
		prices := app.prices
		app.pricesMu.Unlock()
		return prices, nil
	}
	stale := app.prices
	app.pricesMu.Unlock()

	body, header, err := app.esi.FetchRaw(ctx, "", "/markets/prices/")
	if err != nil {
		if len(stale) > 0 {
			logging.Warnf("market: refresh prices failed (%v); serving stale cache", err)
			return stale, nil
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
	app.pricesMu.Lock()
	app.prices = prices
	app.pricesExpiry = expiry
	app.pricesMu.Unlock()
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

// loadMarketItem builds the item view for (type, region): guide
// prices plus the best and top orders of the regional book.
func (app *Application) loadMarketItem(ctx context.Context, typeID, regionID int64, anon bool) (*marketItem, error) {
	item := &marketItem{
		TypeID:    typeID,
		RegionID:  regionID,
		Anonymous: anon,
	}
	if anon {
		// Names from the local SDE: an anonymous view never asks
		// ESI, so a type the SDE doesn't carry has no page.
		t, err := app.queries.GetSDEType(ctx, typeID)
		if err != nil {
			return nil, fmt.Errorf("market: type %d is not in the local data", typeID)
		}
		item.Name = t.Name
	} else {
		item.Name = app.esi.TypeName(ctx, typeID)
	}
	item.RegionName, _ = marketRegionName(regionID)
	// A global-market type (PLEX) has one market; no regions to switch.
	for _, reg := range marketRegions {
		if globalMarketTypes[typeID] {
			break
		}
		item.Regions = append(item.Regions, marketRegion{
			ID:     reg.ID,
			Name:   reg.Name,
			Active: reg.ID == regionID,
		})
	}

	var prices map[int64]esi.MarketPrice
	if anon {
		// The guide as held: an anonymous view never refreshes it.
		app.pricesMu.Lock()
		prices = app.prices
		app.pricesMu.Unlock()
	} else {
		var err error
		if prices, err = app.marketPrices(ctx); err != nil {
			return nil, err
		}
	}
	if p, ok := prices[typeID]; ok {
		if p.AveragePrice > 0 {
			item.AveragePrice = esi.FormatISK(p.AveragePrice)
		}
		if p.AdjustedPrice > 0 {
			item.AdjustedPrice = esi.FormatISK(p.AdjustedPrice)
		}
	}

	// No average: the type trades by contract, not on the
	// market, and CCP's adjusted price for it is a fossil.
	// The stored true value is the honest headline instead.
	if item.AveragePrice == "" {
		if tv, ok := app.storedTrueValues(ctx)[typeID]; ok {
			if est, ok := storedEstimate(tv); ok {
				item.EstimatedSell = esi.FormatISK(est)
				item.EstimatedSellNote = trueValueBasis(tv)
			}
		}
	}

	var sells, buys []esi.MarketOrder
	var truncated bool
	// fromSnapshot: the book is the stored sweep snapshot (top
	// orders only), so the whole-book figures (depth, typicals,
	// bands) come from the region stats row, not from these rows.
	var fromSnapshot *db.MarketRegionStat
	if anon {
		// First choice: a book a signed-in view fetched lately.
		held, ok := app.books.get(marketKey{RegionID: regionID, TypeID: typeID}, time.Now())
		if ok {
			sells, buys, truncated = append([]esi.MarketOrder(nil), held.sells...), append([]esi.MarketOrder(nil), held.buys...), held.truncated
		} else if !app.anonMarketBlocked() {
			// The stored book: the top orders the last completed
			// region sweep kept, with the sweep's own stats row.
			// A region+type no sweep has covered stays pending.
			stored, stat, sok := app.storedBook(ctx, regionID, typeID)
			if !sok {
				item.BookPending = true
				return item, nil
			}
			sells, buys = stored.sells, stored.buys
			fromSnapshot = stat
			item.BookStoredAt = ageText(time.Now(), stat.UpdatedAt)
		} else {
			item.BookPending = true
			return item, nil
		}
	} else {
		var err error
		if sells, buys, truncated, err = app.fetchOrderBook(ctx, regionID, typeID); err != nil {
			return nil, err
		}
	}
	item.Truncated = truncated
	if fromSnapshot != nil {
		item.SellOrders = int(fromSnapshot.SellOrders)
		item.BuyOrders = int(fromSnapshot.BuyOrders)
		item.SellVolume = esi.FormatInt(fromSnapshot.SellVolume)
		item.BuyVolume = esi.FormatInt(fromSnapshot.BuyVolume)
	} else {
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
	}

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
		if !anon {
			app.noteStructureIDs(ctx, ids...)
		}
	}

	if len(sells) > 0 {
		item.BestSellRaw = sells[0].Price
		item.BestSell = esi.FormatISK(sells[0].Price)
		item.BestSellLoc = app.orderPlace(ctx, sells[0].LocationID, sells[0].SystemID)
	}
	if len(buys) > 0 {
		item.BestBuyRaw = buys[0].Price
		item.BestBuy = esi.FormatISK(buys[0].Price)
		item.BestBuyLoc = app.orderPlace(ctx, buys[0].LocationID, buys[0].SystemID)
	}
	// The typicals come first: the spread below is priced from
	// them, so the card never quotes a gap the book's own median
	// orders do not support.
	var typicalSell, typicalBuy float64
	if fromSnapshot != nil {
		// The sweep's own whole-book figures: summarizing the
		// top few stored orders would invent a median the full
		// book never had.
		typicalSell, typicalBuy = fromSnapshot.TypicalSell, fromSnapshot.TypicalBuy
		if typicalSell > 0 {
			item.TypicalSell = esi.FormatISK(typicalSell)
			item.SellBand = esi.FormatISK(fromSnapshot.SellBand)
		}
		if typicalBuy > 0 {
			item.TypicalBuy = esi.FormatISK(typicalBuy)
			item.BuyBand = esi.FormatISK(fromSnapshot.BuyBand)
		}
	} else {
		if stats, ok := summarizeBook(sells); ok {
			typicalSell = stats.Median
			item.TypicalSell = esi.FormatISK(stats.Median)
			item.SellBand = esi.FormatISK(stats.High90)
		}
		if stats, ok := summarizeBook(buys); ok {
			typicalBuy = stats.Median
			item.TypicalBuy = esi.FormatISK(stats.Median)
			item.BuyBand = esi.FormatISK(stats.Low90)
		}
	}

	// The spread card: typical against typical, and only from a
	// book deep enough for a median to mean anything. At three
	// or fewer orders a side the "typical" is still one order's
	// price wearing a median's clothes, so the card names the
	// thin book instead of printing a percentage the extremes
	// manufactured (one hopeful ask over a deep bid side used
	// to read as a 2000% spread).
	if item.SellOrders > 0 && item.BuyOrders > 0 {
		switch {
		case item.SellOrders <= spreadBookDepth || item.BuyOrders <= spreadBookDepth:
			item.SpreadThin = thinBookNote(item.SellOrders, item.BuyOrders)
		case typicalBuy > 0:
			pct := (typicalSell - typicalBuy) / typicalBuy * 100
			item.Spread = fmt.Sprintf("%.1f%%", pct)
			item.SpreadBand = spreadBand(pct)
		}
	}

	for i, o := range sells {
		if i >= topMarketOrders {
			break
		}
		item.Sells = append(item.Sells, marketOrderRow{
			Price:  esi.FormatISK(o.Price) + " ISK",
			Volume: esi.FormatInt(o.VolumeRemain),
			Loc:    app.orderPlace(ctx, o.LocationID, o.SystemID),
		})
	}
	for i, o := range buys {
		if i >= topMarketOrders {
			break
		}
		item.Buys = append(item.Buys, marketOrderRow{
			Price:  esi.FormatISK(o.Price) + " ISK",
			Volume: esi.FormatInt(o.VolumeRemain),
			Loc:    app.orderPlace(ctx, o.LocationID, o.SystemID),
		})
	}
	return item, nil
}

// storedBook is one type's stored sweep book: the top orders the
// last completed region sweep kept (market_book_orders), split
// into sides in display order, plus the sweep's region stats row
// (the whole-book figures those top rows cannot speak for). ok is
// false when no sweep has stored a book for this region+type --
// a stats row alone is not a book.
type storedBook struct {
	sells, buys []esi.MarketOrder
}

func (app *Application) storedBook(ctx context.Context, regionID, typeID int64) (storedBook, *db.MarketRegionStat, bool) {
	stat, err := app.queries.GetMarketRegionStat(ctx, db.GetMarketRegionStatParams{RegionID: regionID, TypeID: typeID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			logging.Errorf("market: region stat for type %d in region %d: %v", typeID, regionID, err)
		}
		return storedBook{}, nil, false
	}
	rows, err := app.queries.ListMarketBookOrders(ctx, db.ListMarketBookOrdersParams{RegionID: regionID, TypeID: typeID})
	if err != nil {
		logging.Errorf("market: stored book for type %d in region %d: %v", typeID, regionID, err)
		return storedBook{}, nil, false
	}
	if len(rows) == 0 {
		return storedBook{}, nil, false
	}
	book := storedBook{}
	for _, row := range rows {
		o := esi.MarketOrder{
			TypeID: typeID, Price: row.Price, VolumeRemain: row.VolumeRemain,
			LocationID: row.LocationID, SystemID: row.SystemID, IsBuyOrder: row.IsBuyOrder == 1,
		}
		if o.IsBuyOrder {
			book.buys = append(book.buys, o)
		} else {
			book.sells = append(book.sells, o)
		}
	}
	return book, &stat, true
}

// orderPlace is orderLocation classified for the link policy: an
// NPC station or system the SDE knows links to its page; a player
// structure stays text. Cache-only on top of orderLocation's own
// resolution.
func (app *Application) orderPlace(ctx context.Context, locationID, systemID int64) placeRef {
	id := locationID
	if id == 0 {
		id = systemID
	}
	return app.linkPlace(ctx, id, app.orderLocation(ctx, locationID, systemID))
}

// orderLocation renders where an order sits: the NPC station name
// when the location is one, the solar-system name for system-level
// orders (and as fallback), and for player structures the name
// the worker has resolved (structures.go) or a
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
// table, and a placeholder for ids neither knows.
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
			logging.Errorf("market: watch search %q: %v", watchQuery, err)
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
		logging.Errorf("market: list watchlist for user %d: %v", userID, err)
		return view
	}
	view.Rows = app.watchlistRows(ctx, entries)
	return view
}

// watchlistRows renders stored watchlist entries: latest
// average, 7/30-day moves, and the moving flag past the user's
// line. Split from buildWatchlistView so Home's watchlist widget
// renders the render's shared fetch instead of asking again.
func (app *Application) watchlistRows(ctx context.Context, entries []db.MarketWatchlist) []watchlistRow {
	var out []watchlistRow
	for _, e := range entries {
		row := watchlistRow{
			TypeID:    e.TypeID,
			Name:      app.typeNameOrID(ctx, e.TypeID),
			RegionID:  e.RegionID,
			Region:    app.marketRegionLabel(ctx, marketRegionForType(e.TypeID, e.RegionID)),
			Current:   "—",
			Change7:   "—",
			Change30:  "—",
			Threshold: fmt.Sprintf("%g", e.ThresholdPct),
		}
		// The stat pack's typical sell where a sweep covers the
		// region, the traded averages elsewhere (watchMove): the
		// same figures the watch alerts fire on.
		m := app.watchMove(ctx, e.TypeID, marketRegionForType(e.TypeID, e.RegionID))
		if m.Has {
			row.Current = esi.FormatISK(m.Current)
		}
		if m.Has7 {
			row.Change7 = markethistory.FormatChangePct(m.Chg7)
			if math.Abs(m.Chg7) >= e.ThresholdPct {
				row.Moving = true
				row.MoveText = markethistory.ChangeDirection(m.Chg7) + " over 7 days"
			}
		}
		if m.Has30 {
			row.Change30 = markethistory.FormatChangePct(m.Chg30)
		}
		out = append(out, row)
	}
	return out
}

// buildYourOrders assembles the "Your orders" section: every open
// sell order across the user's characters with the health verdict
// the worker computed (or a "not checked yet" line before the
// first book pass lands).
func (app *Application) buildYourOrders(ctx context.Context, userID int64) []yourOrderRow {
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("market: your orders: list characters for user %d: %v", userID, err)
		return nil
	}
	health := make(map[int64]db.OrderHealth)
	if rows, err := app.queries.ListOrderHealthByUser(ctx, userID); err != nil {
		logging.Errorf("market: your orders: list health for user %d: %v", userID, err)
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
				Char:   ch.Name,
				CharID: ch.CharacterID,
				Item:   app.typeNameOrID(ctx, o.TypeID),
				TypeID: o.TypeID,
				Price:  esi.FormatISK(o.Price) + " ISK",
				Loc:    app.linkPlace(ctx, o.LocationID, app.econLocationTitle(ctx, o.LocationID)),
				Status: "Not checked against the order book yet",
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
	userID := app.userID(ctx)
	next := "/market/"
	if err := r.ParseForm(); err == nil {
		if n := r.Form.Get("next"); safeLocalNext(n) {
			next = n
		}
		if userID > 0 {
			typeID, _ := strconv.ParseInt(r.Form.Get("type"), 10, 64)
			regionID, _ := strconv.ParseInt(r.Form.Get("region"), 10, 64)
			if regionID <= 0 {
				regionID = defaultMarketRegion
			}
			if r.Form.Get("action") != "remove" {
				regionID = marketRegionForType(typeID, regionID)
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
						CreatedAt:    time.Now().UTC(),
					}); err != nil {
						logging.Errorf("market: watch upsert type %d for user %d: %v", typeID, userID, err)
					}
				}
			case "remove":
				if typeID > 0 {
					if err := app.queries.DeleteWatchlistEntry(ctx, db.DeleteWatchlistEntryParams{
						UserID: userID, TypeID: typeID, RegionID: regionID,
					}); err != nil {
						logging.Errorf("market: watch remove type %d for user %d: %v", typeID, userID, err)
					}
				}
			}
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
