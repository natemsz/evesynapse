package app

// The LP store browser (/market/lp/): every mirrored NPC loyalty
// offer valued in a hub region, best ISK per LP first. It reads the
// stored offer values only (lp_offer_values, written from the stat
// pack by market_lp.go); the ranking runs in SQL with a LIMIT, and
// the page formats at most marketLPRowCap rows.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/contracts"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const marketLPRowCap = 100

type marketLPRequirement struct {
	TypeID   int64
	Name     string
	Quantity string
	IconURL  string // the item's icon on the EVE image server
	Href     string // the item's market page, in the view's region
	TipLines string // tooltip rows for the shared tooltip builder
	Sell     string // the stored sell figure per unit, for the inspector
}

type marketLPRow struct {
	TypeID       int64
	Name         string
	Store        string
	StoreID      int64
	Quantity     string
	LPCost       string
	ISKCost      string
	Requirements []marketLPRequirement
	Value        string // product value at the chosen side
	ValueShort   string // the same, compact for the table (14.8M)
	ISKCostShort string
	ISKPerLP     string
	ISKPerLPDir  string // "up" when the offer pays, "down" when it loses
	TradedDay    string
	Chg7         trendFigure
	Age          string
	RequiredCost string // what the also-needs items cost at their sell, "" with none

	IconURL  string // the product icon, by SDE category (blueprints and SKINs differ)
	OfferID  int64
	Href     string // this offer in the inspector, the view's filters kept
	On       bool   // the offer open in the inspector
	MeterPct string // ISK per LP against the view's best, 0 to 100
	lpCost   int64
	iskPerLP float64
	priced   bool
}

type marketLPStore struct {
	ID     int64
	Name   string
	Active bool
}

type marketLPView struct {
	RegionID    int64
	RegionName  string
	Regions     []marketRegionChoice
	Stores      []marketLPStore
	StoreID     int64
	Instant     bool // value at the typical buy (sell at once) instead of the typical sell
	MinVolume   int64
	MinISKPerLP float64
	MaxISKPerLP float64
	MinLP       int64
	MaxLP       int64
	Item        string // find-stores search text, as typed
	ExportQuery string // the active filters as a query string, for the export links
	HasStores   bool
	Unpriced    int64
	Rows        []marketLPRow

	// The framework console: the filters and the reader's LP are
	// its directory, the offers its main pane, one offer its
	// inspector. Depth comes from the URL: 1 for the offers, 2 with
	// ?offer=<corp>-<offer> open. The directory never stands alone:
	// on a narrow page it rides above the offers (lp.css), the way
	// the phone mock stacks the filters over the list.
	Depth      int
	Offer      string // the open offer's key, "" with none
	ListHref   string // the view with no offer open
	Chips      []marketLPChip
	Balances   []marketLPBalance
	SignedIn   bool
	Insp       *marketLPRow
	InspLP     []marketLPPilotLP // the reader's pilots' LP with the open offer's store
	InspAfford string            // redemptions that LP covers, "" when unknown
}

// marketLPChip is one active filter above the offers, with a link
// that drops it.
type marketLPChip struct {
	Label string
	Href  string
}

// marketLPBalance is the reader's LP with one store, summed over
// their pilots; picking it filters the offers to that store.
type marketLPBalance struct {
	StoreID  int64
	Store    string
	Points   string
	Pilots   []marketLPPilotLP
	Href     string
	On       bool
	MeterPct string
	points   int64
}

type marketLPPilotLP struct {
	Name   string
	Points string
	points int64
}

func (app *Application) handleMarketLP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored market data only
	view := app.buildMarketLPView(ctx, r.URL.Query())
	if view.HasStores {
		app.attachLPBalances(ctx, view, app.userID(ctx))
	}
	view.finishConsole(r.URL.Query())
	data.MarketLP = view
	app.render(ctx, w, http.StatusOK, "market_lp.html", data)
}

func (app *Application) buildMarketLPView(ctx context.Context, q map[string][]string) *marketLPView {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	view := &marketLPView{RegionID: defaultMarketRegion, MinVolume: 10, Instant: get("side") == "buy"}
	if id, err := strconv.ParseInt(get("region"), 10, 64); err == nil {
		if _, ok := marketRegionName(id); ok {
			view.RegionID = id
		}
	}
	view.RegionName, _ = marketRegionName(view.RegionID)
	if v, err := strconv.ParseInt(get("minvol"), 10, 64); err == nil && v >= 0 {
		view.MinVolume = v
	}
	if v, err := strconv.ParseInt(get("corp"), 10, 64); err == nil && v > 0 {
		view.StoreID = v
	}
	if v, err := strconv.ParseFloat(get("minisklp"), 64); err == nil && v > 0 {
		view.MinISKPerLP = v
	}
	if v, err := strconv.ParseFloat(get("maxisklp"), 64); err == nil && v > 0 {
		view.MaxISKPerLP = v
	}
	if v, err := strconv.ParseInt(get("minlp"), 10, 64); err == nil && v > 0 {
		view.MinLP = v
	}
	if v, err := strconv.ParseInt(get("maxlp"), 10, 64); err == nil && v > 0 {
		view.MaxLP = v
	}
	view.Item = get("item")
	for _, region := range marketRegions {
		view.Regions = append(view.Regions, marketRegionChoice{ID: region.ID, Name: region.Name, Active: region.ID == view.RegionID})
	}
	stores, err := app.queries.ListLPStoreCorps(ctx)
	if err != nil {
		logging.Errorf("lp store: store list: %v", err)
		return view
	}
	known := false
	for _, s := range stores {
		name := s.Name
		if name == "" {
			name = "Corporation " + strconv.FormatInt(s.CorporationID, 10)
		}
		view.Stores = append(view.Stores, marketLPStore{ID: s.CorporationID, Name: name, Active: s.CorporationID == view.StoreID})
		known = known || s.CorporationID == view.StoreID
	}
	if !known {
		view.StoreID = 0
	}
	view.HasStores = len(stores) > 0
	if !view.HasStores {
		return view
	}

	params := view.lpParams(marketLPRowCap)
	// Find-stores: an item name resolves to its types and the list
	// becomes every store's offer for them. A name that matches no
	// type searches for a type that does not exist, so the answer
	// is an honest empty list, not every offer.
	if len(view.Item) >= 2 {
		params.TypeIds = []int64{-1}
		if found, err := app.queries.SuggestSDETypes(ctx, view.Item); err == nil && len(found) > 0 {
			params.TypeIds = params.TypeIds[:0]
			for _, f := range found {
				params.TypeIds = append(params.TypeIds, f.TypeID)
			}
		}
	}
	view.ExportQuery = lpExportQuery(view)
	rows, err := app.queries.ListLPOfferValues(ctx, params)
	if err != nil {
		logging.Errorf("lp store: offers for region %d: %v", view.RegionID, err)
		return view
	}
	if n, err := app.queries.CountLPOfferValuesUnpriced(ctx, db.CountLPOfferValuesUnpricedParams{
		RegionID: view.RegionID, CorporationID: view.StoreID,
	}); err == nil {
		view.Unpriced = n
	}

	corpIDs := make([]int64, 0, len(rows))
	offerIDs := make([]int64, 0, len(rows))
	for _, r := range rows {
		corpIDs = append(corpIDs, r.CorporationID)
		offerIDs = append(offerIDs, r.OfferID)
	}
	type offerKey struct{ corp, offer int64 }
	reqs := map[offerKey][]marketLPRequirement{}
	reqCategories := map[int64]int64{}
	if len(rows) > 0 {
		reqRows, err := app.queries.ListLPOfferRequirementsFor(ctx, db.ListLPOfferRequirementsForParams{
			CorporationIds: corpIDs, OfferIds: offerIDs,
		})
		if err != nil {
			logging.Errorf("lp store: requirements: %v", err)
		}
		// One stored-stat read prices every requirement on the
		// page for the icons' tooltips: the stat pack's averages
		// first, its typicals as the fallback, an honest gap
		// when neither is stored.
		prices := map[int64]db.ListLPRequirementPricesRow{}
		if len(reqRows) > 0 {
			ids := make([]int64, 0, len(reqRows))
			seen := map[int64]bool{}
			for _, q := range reqRows {
				if !seen[q.TypeID] {
					seen[q.TypeID] = true
					ids = append(ids, q.TypeID)
				}
			}
			if priceRows, err := app.queries.ListLPRequirementPrices(ctx, db.ListLPRequirementPricesParams{
				RegionID: view.RegionID, TypeIds: ids,
			}); err == nil {
				for _, p := range priceRows {
					prices[p.TypeID] = p
				}
			} else {
				logging.Errorf("lp store: requirement prices: %v", err)
			}
			// The requirement icons resolve by SDE category:
			// the image server serves no /icon for blueprint
			// types and no picture at all for SKIN types.
			if catRows, err := app.queries.ListSDETypeCategories(ctx, ids); err == nil {
				for _, r := range catRows {
					reqCategories[r.TypeID] = r.CategoryID
				}
			} else {
				logging.Errorf("lp store: requirement categories: %v", err)
			}
		}
		for _, q := range reqRows {
			k := offerKey{q.CorporationID, q.OfferID}
			req := marketLPRequirement{
				TypeID:   q.TypeID,
				Name:     app.typeNameOrID(ctx, q.TypeID),
				Quantity: esi.FormatInt(q.Quantity),
				IconURL:  contracts.TypeCategoryIconURL(q.TypeID, reqCategories[q.TypeID], 32),
				Href:     "/market/?type=" + strconv.FormatInt(q.TypeID, 10) + "&region=" + strconv.FormatInt(view.RegionID, 10),
			}
			sell, buy := "no market", "no market"
			if p, ok := prices[q.TypeID]; ok {
				if v := firstStoredPrice(p.SellAvg, p.TypicalSell); v > 0 {
					sell = esi.FormatISK(v)
				}
				if v := firstStoredPrice(p.BuyAvg, p.TypicalBuy); v > 0 {
					buy = esi.FormatISK(v)
				}
			}
			req.TipLines = "Needs|" + req.Quantity + ";Avg sell|" + sell + ";Avg buy|" + buy
			req.Sell = sell
			reqs[k] = append(reqs[k], req)
		}
	}
	// The product icons resolve by SDE category, as the
	// requirement icons do.
	rowCategories := map[int64]int64{}
	if len(rows) > 0 {
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.TypeID)
		}
		if catRows, err := app.queries.ListSDETypeCategories(ctx, ids); err == nil {
			for _, c := range catRows {
				rowCategories[c.TypeID] = c.CategoryID
			}
		} else {
			logging.Errorf("lp store: offer categories: %v", err)
		}
	}
	now := time.Now()
	for _, r := range rows {
		unit, perLP := r.UnitSell, r.IskPerLpSell
		if view.Instant {
			unit, perLP = r.UnitBuy, r.IskPerLpBuy
		}
		store := r.CorporationName
		if store == "" {
			store = "Corporation " + strconv.FormatInt(r.CorporationID, 10)
		}
		row := marketLPRow{
			TypeID: r.TypeID, Name: app.typeNameOrID(ctx, r.TypeID),
			Store: store, StoreID: r.CorporationID,
			Quantity: esi.FormatInt(r.Quantity), LPCost: esi.FormatInt(r.LpCost),
			ISKCost:      esi.FormatISK(r.IskCost),
			Requirements: reqs[offerKey{r.CorporationID, r.OfferID}],
			Value:        esi.FormatISK(unit * float64(r.Quantity)),
			ValueShort:   formatISKShort(unit * float64(r.Quantity)),
			ISKCostShort: formatISKShort(r.IskCost),
			ISKPerLP:     esi.FormatISK(perLP.Float64),
			TradedDay:    esi.FormatInt(int64(math.Round(r.DailyVolume))),
			Chg7:         newTrendFigure(r.SellChg7d),
			OfferID:      r.OfferID,
			IconURL:      contracts.TypeCategoryIconURL(r.TypeID, rowCategories[r.TypeID], 32),
			lpCost:       r.LpCost,
			iskPerLP:     perLP.Float64,
			priced:       perLP.Valid,
		}
		if perLP.Valid && perLP.Float64 > 0 {
			row.ISKPerLPDir = "up"
		} else if perLP.Valid {
			row.ISKPerLPDir = "down"
		}
		if r.RequiredCost > 0 {
			total := esi.FormatISK(r.RequiredCost)
			row.RequiredCost = total
			for i := range row.Requirements {
				row.Requirements[i].TipLines += ";Items total|" + total
			}
		}
		if r.PricesAt.Valid {
			row.Age = ageText(now, r.PricesAt.Time)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// lpOfferKey names one offer in the URL: the store, then the offer.
func lpOfferKey(corp, offer int64) string {
	return strconv.FormatInt(corp, 10) + "-" + strconv.FormatInt(offer, 10)
}

// attachLPBalances reads the signed-in reader's stored LP balances
// (the loyalty snapshot each pilot syncs) for the stores the
// browser knows, summed per store, largest first. Nobody signed in,
// or no pilot with the scope, leaves the list empty.
func (app *Application) attachLPBalances(ctx context.Context, view *marketLPView, userID int64) {
	if userID == 0 {
		return
	}
	view.SignedIn = true
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("lp store: characters: %v", err)
		return
	}
	names := map[int64]string{}
	for _, s := range view.Stores {
		names[s.ID] = s.Name
	}
	byStore := map[int64]*marketLPBalance{}
	for _, ch := range chars {
		snap, err := app.queries.GetSnapshot(ctx, db.GetSnapshotParams{CharacterID: ch.CharacterID, Kind: esi.SnapLoyalty})
		if err != nil {
			continue
		}
		var balances esi.LoyaltyBalances
		if json.Unmarshal([]byte(snap.Payload), &balances) != nil {
			continue
		}
		for _, b := range balances {
			name, ok := names[b.CorporationID]
			if !ok || b.LoyaltyPoints <= 0 {
				continue
			}
			bal := byStore[b.CorporationID]
			if bal == nil {
				bal = &marketLPBalance{StoreID: b.CorporationID, Store: name}
				byStore[b.CorporationID] = bal
			}
			bal.points += b.LoyaltyPoints
			bal.Pilots = append(bal.Pilots, marketLPPilotLP{Name: ch.Name, Points: esi.FormatInt(b.LoyaltyPoints), points: b.LoyaltyPoints})
		}
	}
	var top int64
	for _, bal := range byStore {
		bal.Points = esi.FormatInt(bal.points)
		slices.SortFunc(bal.Pilots, func(a, b marketLPPilotLP) int { return cmp.Compare(b.points, a.points) })
		view.Balances = append(view.Balances, *bal)
		top = max(top, bal.points)
	}
	slices.SortFunc(view.Balances, func(a, b marketLPBalance) int {
		if c := cmp.Compare(b.points, a.points); c != 0 {
			return c
		}
		return strings.Compare(a.Store, b.Store)
	})
	for i := range view.Balances {
		b := &view.Balances[i]
		b.On = b.StoreID == view.StoreID
		if top > 0 {
			b.MeterPct = strconv.FormatInt(b.points*100/top, 10)
		}
	}
}

// finishConsole places the reader in the console from the URL and
// fills the links that move between its levels and drop filters.
func (v *marketLPView) finishConsole(q url.Values) {
	base := url.Values{}
	if v.HasStores {
		base, _ = url.ParseQuery(lpExportQuery(v))
	}
	href := func(vals url.Values) string {
		if e := vals.Encode(); e != "" {
			return "/market/lp/?" + e
		}
		return "/market/lp/"
	}
	with := func(k, val string) string {
		vals := url.Values{}
		for key, vs := range base {
			vals[key] = vs
		}
		if val == "" {
			vals.Del(k)
		} else {
			vals.Set(k, val)
		}
		return href(vals)
	}
	v.ListHref = href(base)
	v.Depth = 1
	v.Offer = strings.TrimSpace(q.Get("offer"))
	var best float64
	for _, r := range v.Rows {
		best = max(best, r.iskPerLP)
	}
	for i := range v.Rows {
		r := &v.Rows[i]
		key := lpOfferKey(r.StoreID, r.OfferID)
		r.Href = with("offer", key)
		if best > 0 && r.priced && r.iskPerLP > 0 {
			r.MeterPct = strconv.FormatInt(int64(math.Round(r.iskPerLP*100/best)), 10)
		} else {
			r.MeterPct = "0"
		}
		if v.Offer != "" && key == v.Offer {
			r.On = true
			v.Insp = r
		}
	}
	if v.Insp != nil {
		v.Depth = 2
		for _, b := range v.Balances {
			if b.StoreID != v.Insp.StoreID {
				continue
			}
			v.InspLP = b.Pilots
			if v.Insp.lpCost > 0 {
				var n int64
				for _, p := range b.Pilots {
					n += p.points / v.Insp.lpCost
				}
				v.InspAfford = esi.FormatInt(n)
			}
		}
	} else {
		v.Offer = ""
	}
	for i := range v.Balances {
		b := &v.Balances[i]
		if b.On {
			b.Href = with("corp", "")
		} else {
			b.Href = with("corp", strconv.FormatInt(b.StoreID, 10))
		}
	}
	// The active filters, each with a link that drops it.
	for _, s := range v.Stores {
		if s.Active {
			v.Chips = append(v.Chips, marketLPChip{Label: "Store: " + s.Name, Href: with("corp", "")})
		}
	}
	if v.Item != "" {
		v.Chips = append(v.Chips, marketLPChip{Label: "Item: " + v.Item, Href: with("item", "")})
	}
	if v.Instant {
		v.Chips = append(v.Chips, marketLPChip{Label: "Selling to buy orders", Href: with("side", "")})
	}
	if v.MinVolume != 10 {
		v.Chips = append(v.Chips, marketLPChip{Label: "Sold per day ≥ " + esi.FormatInt(v.MinVolume), Href: with("minvol", "")})
	}
	if v.MinISKPerLP > 0 {
		v.Chips = append(v.Chips, marketLPChip{Label: "ISK per LP ≥ " + strconv.FormatFloat(v.MinISKPerLP, 'f', -1, 64), Href: with("minisklp", "")})
	}
	if v.MaxISKPerLP > 0 {
		v.Chips = append(v.Chips, marketLPChip{Label: "ISK per LP ≤ " + strconv.FormatFloat(v.MaxISKPerLP, 'f', -1, 64), Href: with("maxisklp", "")})
	}
	if v.MinLP > 0 {
		v.Chips = append(v.Chips, marketLPChip{Label: "LP ≥ " + esi.FormatInt(v.MinLP), Href: with("minlp", "")})
	}
	if v.MaxLP > 0 {
		v.Chips = append(v.Chips, marketLPChip{Label: "LP ≤ " + esi.FormatInt(v.MaxLP), Href: with("maxlp", "")})
	}
}

// firstStoredPrice is the first usable figure of a stored pair:
// the stat pack's average where it stored one, else its typical.
func firstStoredPrice(avg, typical float64) float64 {
	if avg > 0 {
		return avg
	}
	return typical
}

// lpParams is the stored-values query for this view at a row cap.
func (v *marketLPView) lpParams(cap int64) db.ListLPOfferValuesParams {
	p := db.ListLPOfferValuesParams{
		RegionID: v.RegionID, CorporationID: v.StoreID, MinVolume: float64(v.MinVolume),
		Instant: v.Instant, RowCap: cap, TypeIds: []int64{},
	}
	if v.MinISKPerLP > 0 {
		p.MinIskLp = sql.NullFloat64{Float64: v.MinISKPerLP, Valid: true}
	}
	if v.MaxISKPerLP > 0 {
		p.MaxIskLp = sql.NullFloat64{Float64: v.MaxISKPerLP, Valid: true}
	}
	if v.MinLP > 0 {
		p.MinLp = sql.NullInt64{Int64: v.MinLP, Valid: true}
	}
	if v.MaxLP > 0 {
		p.MaxLp = sql.NullInt64{Int64: v.MaxLP, Valid: true}
	}
	return p
}

// lpExportQuery renders the active filters as the query string the
// export endpoints take, so an export matches the page it came from.
func lpExportQuery(v *marketLPView) string {
	q := url.Values{}
	q.Set("region", strconv.FormatInt(v.RegionID, 10))
	if v.StoreID > 0 {
		q.Set("corp", strconv.FormatInt(v.StoreID, 10))
	}
	if v.Instant {
		q.Set("side", "buy")
	}
	q.Set("minvol", strconv.FormatInt(v.MinVolume, 10))
	if v.MinISKPerLP > 0 {
		q.Set("minisklp", strconv.FormatFloat(v.MinISKPerLP, 'f', -1, 64))
	}
	if v.MaxISKPerLP > 0 {
		q.Set("maxisklp", strconv.FormatFloat(v.MaxISKPerLP, 'f', -1, 64))
	}
	if v.MinLP > 0 {
		q.Set("minlp", strconv.FormatInt(v.MinLP, 10))
	}
	if v.MaxLP > 0 {
		q.Set("maxlp", strconv.FormatInt(v.MaxLP, 10))
	}
	if v.Item != "" {
		q.Set("item", v.Item)
	}
	return q.Encode()
}

const marketLPExportCap = 5000

// handleMarketLPExportCSV downloads the current LP view as CSV:
// the same filters the page takes, at an export-sized row cap.
func (app *Application) handleMarketLPExportCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := app.buildMarketLPView(ctx, r.URL.Query())
	params := view.lpParams(marketLPExportCap)
	if len(view.Item) >= 2 {
		params.TypeIds = []int64{-1}
		if found, err := app.queries.SuggestSDETypes(ctx, view.Item); err == nil && len(found) > 0 {
			params.TypeIds = params.TypeIds[:0]
			for _, f := range found {
				params.TypeIds = append(params.TypeIds, f.TypeID)
			}
		}
	}
	rows, err := app.queries.ListLPOfferValues(ctx, params)
	if err != nil {
		logging.Errorf("lp store: csv export: %v", err)
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="evesynapse-lp-offers.csv"`)
	out := csv.NewWriter(w)
	_ = out.Write([]string{"item", "type_id", "store", "corporation_id", "quantity", "lp_cost", "isk_cost", "required_items_cost", "value", "isk_per_lp", "sold_per_day", "region_id", "side"})
	for _, row := range rows {
		unit, perLP := row.UnitSell, row.IskPerLpSell
		if view.Instant {
			unit, perLP = row.UnitBuy, row.IskPerLpBuy
		}
		side := "sell"
		if view.Instant {
			side = "buy"
		}
		_ = out.Write([]string{
			app.typeNameOrID(ctx, row.TypeID),
			strconv.FormatInt(row.TypeID, 10),
			row.CorporationName,
			strconv.FormatInt(row.CorporationID, 10),
			strconv.FormatInt(row.Quantity, 10),
			strconv.FormatInt(row.LpCost, 10),
			strconv.FormatFloat(row.IskCost, 'f', 2, 64),
			strconv.FormatFloat(row.RequiredCost, 'f', 2, 64),
			strconv.FormatFloat(unit*float64(row.Quantity), 'f', 2, 64),
			strconv.FormatFloat(perLP.Float64, 'f', 2, 64),
			strconv.FormatFloat(row.DailyVolume, 'f', 2, 64),
			strconv.FormatInt(view.RegionID, 10),
			side,
		})
	}
	out.Flush()
}

// handleMarketLPExportJSON is the CSV's JSON twin: the same rows
// with numeric fields, for tools that read the store directly.
func (app *Application) handleMarketLPExportJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := app.buildMarketLPView(ctx, r.URL.Query())
	params := view.lpParams(marketLPExportCap)
	if len(view.Item) >= 2 {
		params.TypeIds = []int64{-1}
		if found, err := app.queries.SuggestSDETypes(ctx, view.Item); err == nil && len(found) > 0 {
			params.TypeIds = params.TypeIds[:0]
			for _, f := range found {
				params.TypeIds = append(params.TypeIds, f.TypeID)
			}
		}
	}
	rows, err := app.queries.ListLPOfferValues(ctx, params)
	if err != nil {
		logging.Errorf("lp store: json export: %v", err)
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}
	type offer struct {
		Item          string  `json:"item"`
		TypeID        int64   `json:"type_id"`
		Store         string  `json:"store"`
		CorporationID int64   `json:"corporation_id"`
		Quantity      int64   `json:"quantity"`
		LPCost        int64   `json:"lp_cost"`
		ISKCost       float64 `json:"isk_cost"`
		RequiredCost  float64 `json:"required_items_cost"`
		Value         float64 `json:"value"`
		ISKPerLP      float64 `json:"isk_per_lp"`
		SoldPerDay    float64 `json:"sold_per_day"`
	}
	out := make([]offer, 0, len(rows))
	for _, row := range rows {
		unit, perLP := row.UnitSell, row.IskPerLpSell
		if view.Instant {
			unit, perLP = row.UnitBuy, row.IskPerLpBuy
		}
		out = append(out, offer{
			Item: app.typeNameOrID(ctx, row.TypeID), TypeID: row.TypeID,
			Store: row.CorporationName, CorporationID: row.CorporationID,
			Quantity: row.Quantity, LPCost: row.LpCost, ISKCost: row.IskCost,
			RequiredCost: row.RequiredCost, Value: unit * float64(row.Quantity),
			ISKPerLP: perLP.Float64, SoldPerDay: row.DailyVolume,
		})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"region_id": view.RegionID,
		"side":      map[bool]string{true: "buy", false: "sell"}[view.Instant],
		"offers":    out,
	})
}
