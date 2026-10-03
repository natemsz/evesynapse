package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
// plus the order book for one region.
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
}

// marketView is the Market page body.
type marketView struct {
	Query   string
	Region  int64 // active region (search results link into it)
	Matches []marketMatch
	Item    *marketItem
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

	if typeID, err := strconv.ParseInt(q.Get("type"), 10, 64); err == nil && typeID > 0 {
		item, err := app.loadMarketItem(ctx, typeID, view.Region)
		if err != nil {
			log.Printf("market: load type %d in region %d: %v", typeID, view.Region, err)
			data.Error = "Market data unavailable right now — please try again shortly."
		} else {
			view.Item = item
		}
	} else if view.Query != "" {
		view.Matches = app.searchTypes(ctx, view.Query)
	}

	app.render(w, http.StatusOK, "market.html", data)
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
// orders (and as fallback), and an honest "Structure #<id>" for
// player structures, whose names need a scope this app doesn't
// hold. Station/system names reuse the shared place-name cache.
func (app *Application) orderLocation(ctx context.Context, locationID, systemID int64) string {
	// Upwell structure IDs live up around 1e12, far above the
	// station (6e7) and system (3e7) ranges.
	if locationID > 1_000_000_000 {
		return fmt.Sprintf("Structure #%d", locationID)
	}
	if locationID != 0 && locationID != systemID {
		if name := app.esi.PlaceName(ctx, fmt.Sprintf("/universe/stations/%d/", locationID), locationID, ""); name != "" {
			return name
		}
	}
	return app.esi.PlaceName(ctx, fmt.Sprintf("/universe/systems/%d/", systemID), systemID, fmt.Sprintf("System #%d", systemID))
}
