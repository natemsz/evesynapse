package app

// The LP store browser (/market/lp/): every mirrored NPC loyalty
// offer valued in a hub region, best ISK per LP first. It reads the
// stored offer values only (lp_offer_values, written from the stat
// pack by market_lp.go); the ranking runs in SQL with a LIMIT, and
// the page formats at most marketLPRowCap rows.

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const marketLPRowCap = 100

type marketLPRequirement struct {
	TypeID   int64
	Name     string
	Quantity string
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
	RequiredCost string
	Value        string // product value at the chosen side
	ISKPerLP     string
	TradedDay    string
	Chg7         trendFigure
	Age          string
}

type marketLPStore struct {
	ID     int64
	Name   string
	Active bool
}

type marketLPView struct {
	RegionID   int64
	RegionName string
	Regions    []marketRegionChoice
	Stores     []marketLPStore
	StoreID    int64
	Instant    bool // value at the typical buy (sell at once) instead of the typical sell
	MinVolume  int64
	HasStores  bool
	Unpriced   int64
	Rows       []marketLPRow
}

func (app *Application) handleMarketLP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.publicPage(ctx) // public: stored market data only
	data.MarketLP = app.buildMarketLPView(ctx, r.URL.Query())
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

	rows, err := app.queries.ListLPOfferValues(ctx, db.ListLPOfferValuesParams{
		RegionID: view.RegionID, CorporationID: view.StoreID, MinVolume: float64(view.MinVolume),
		Instant: view.Instant, RowCap: marketLPRowCap,
	})
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
	if len(rows) > 0 {
		reqRows, err := app.queries.ListLPOfferRequirementsFor(ctx, db.ListLPOfferRequirementsForParams{
			CorporationIds: corpIDs, OfferIds: offerIDs,
		})
		if err != nil {
			logging.Errorf("lp store: requirements: %v", err)
		}
		for _, q := range reqRows {
			k := offerKey{q.CorporationID, q.OfferID}
			reqs[k] = append(reqs[k], marketLPRequirement{
				TypeID: q.TypeID, Name: app.typeNameOrID(ctx, q.TypeID), Quantity: esi.FormatInt(q.Quantity),
			})
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
			ISKPerLP:     esi.FormatISK(perLP.Float64),
			TradedDay:    esi.FormatInt(int64(math.Round(r.DailyVolume))),
			Chg7:         newTrendFigure(r.SellChg7d),
		}
		if r.RequiredCost > 0 {
			row.RequiredCost = esi.FormatISK(r.RequiredCost)
		}
		if r.PricesAt.Valid {
			row.Age = ageText(now, r.PricesAt.Time)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}
