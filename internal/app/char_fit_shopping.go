package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Fit → Shopping List.
//
// Explodes a saved fit into a priced shopping list: ship + modules +
// charges, aggregated by type, with market prices and a total. This
// closes the loop between "design" (fitting tool) and "acquire".
// ---------------------------------------------------------------------------

type shoppingLine struct {
	TypeID     int64
	Name       string
	URL        string
	Quantity   int64
	UnitPrice  string
	LineCost   string
	PriceKnown bool
}

type shoppingView struct {
	FitName   string
	FitID     string
	Lines     []shoppingLine
	TotalCost string
	Unpriced  int
}

func (app *Application) handleFitShopping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	fitID := chi.URLParam(r, "id")
	if fitID == "" {
		data.Error = "No fit specified."
		app.render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	fitIDNum, err := strconv.ParseInt(fitID, 10, 64)
	if err != nil {
		data.Error = "Invalid fit ID."
		app.render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}

	// Load the fit's items_json. We need the user ID from the session.
	userID := app.userID(ctx)
	row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{
		ID:     fitIDNum,
		UserID: userID,
	})
	if err != nil {
		logging.Errorf("fit shopping: load fit %s: %v", fitID, err)
		data.Error = "Could not load fit."
		app.render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	itemsJSON := row.ItemsJson
	fitName := row.Name

	var doc fitDoc
	if err := json.Unmarshal([]byte(itemsJSON), &doc); err != nil {
		logging.Errorf("fit shopping: parse fit %s: %v", fitID, err)
		data.Error = "Could not parse fit."
		app.render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	if fitName == "" {
		fitName = doc.Name
	}

	// Aggregate typeID → qty: ship × 1 + modules × qty + charges.
	// Charges carry no count in fitDoc (type per weapon only), so we
	// default to 1,000 rounds per charge type — a sane shopping
	// default the user can adjust.
	qtyByType := make(map[int64]int64)
	if doc.ShipTypeID > 0 {
		qtyByType[doc.ShipTypeID]++
	}
	for _, it := range doc.Items {
		if it.TypeID > 0 && it.Qty > 0 {
			qtyByType[it.TypeID] += int64(it.Qty)
		}
	}
	for _, chargeTypeID := range doc.Charges {
		if chargeTypeID > 0 {
			qtyByType[chargeTypeID] += 1000
		}
	}

	// Price each line from the market guide cache.
	priceMap, _ := app.marketPrices(ctx)
	view := &shoppingView{FitName: fitName, FitID: fitID}
	var total float64
	for typeID, qty := range qtyByType {
		line := shoppingLine{
			TypeID:   typeID,
			Name:     app.typeNameOrID(ctx, typeID),
			URL:      fmt.Sprintf("/items/type/%d/", typeID),
			Quantity: qty,
		}
		if mp, ok := priceMap[typeID]; ok && mp.AveragePrice > 0 {
			p := mp.AveragePrice
			line.PriceKnown = true
			line.UnitPrice = isk(p)
			cost := p * float64(qty)
			line.LineCost = isk(cost)
			total += cost
		} else {
			view.Unpriced++
		}
		view.Lines = append(view.Lines, line)
	}
	sort.Slice(view.Lines, func(i, j int) bool {
		return view.Lines[i].Name < view.Lines[j].Name
	})
	view.TotalCost = isk(total)

	data.Shopping = view
	app.render(ctx, w, http.StatusOK, "fit_shopping.html", data)
}
