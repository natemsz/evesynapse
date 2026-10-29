package app

// The LP store browser (/market/lp/): every mirrored NPC loyalty
// offer valued in a hub region, best ISK per LP first. It reads the
// stored offer values only (lp_offer_values, written from the stat
// pack by market_lp.go); the ranking runs in SQL with a LIMIT, and
// the page formats at most marketLPRowCap rows.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
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
	ISKPerLPDir  string // "up" when the offer pays, "down" when it loses
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
		if perLP.Valid && perLP.Float64 > 0 {
			row.ISKPerLPDir = "up"
		} else if perLP.Valid {
			row.ISKPerLPDir = "down"
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
