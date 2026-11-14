package fitting

import (
	"evesynapse/internal/app/module"
	"fmt"
	json "github.com/goccy/go-json"
	"html/template"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
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
	Spark      template.HTML // the unit price's hover card (The Forge's stored 30-day line)
}

type shoppingView struct {
	FitName   string
	FitID     string
	Lines     []shoppingLine
	TotalCost string
	Unpriced  int
}

func (app *tool) handleFitShopping(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}

	fitID := chi.URLParam(r, "id")
	if fitID == "" {
		data.Error = "No fit specified."
		app.deps.Render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	fitIDNum, err := strconv.ParseInt(fitID, 10, 64)
	if err != nil {
		data.Error = "Invalid fit ID."
		app.deps.Render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}

	// Load the fit's items_json. We need the user ID from the session.
	userID := app.deps.UserID(ctx)
	row, err := app.queries.GetLocalFitting(ctx, db.GetLocalFittingParams{
		ID:     fitIDNum,
		UserID: userID,
	})
	if err != nil {
		logging.Errorf("fit shopping: load fit %s: %v", fitID, err)
		data.Error = "Could not load fit."
		app.deps.Render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	itemsJSON := row.ItemsJson
	fitName := row.Name

	var doc fitview.FitDoc
	if err := json.Unmarshal([]byte(itemsJSON), &doc); err != nil {
		logging.Errorf("fit shopping: parse fit %s: %v", fitID, err)
		data.Error = "Could not parse fit."
		app.deps.Render(ctx, w, http.StatusOK, "fit_shopping.html", data)
		return
	}
	if fitName == "" {
		fitName = doc.Name
	}

	// Aggregate typeID → qty: ship × 1 + modules × qty + charges.
	// Charges carry no count in fitview.FitDoc (type per weapon only), so we
	// default to 1,000 rounds per charge type, a sane shopping
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
	for _, p := range doc.LoadedChargePairs() {
		qtyByType[p.Charge] += 1000
	}

	// Price each line in the house order (fitview.FitUnitPrice):
	// the swept typical sell (The Forge's book, the global book
	// for global-market types), then the stored estimated sell,
	// then the stored build cost. The guide's average prices
	// nothing.
	typicals := app.deps.TypicalSells(ctx)
	trueValues := app.deps.TrueValues(ctx)
	buildCosts := app.deps.BuildCosts(ctx)
	view := &shoppingView{FitName: fitName, FitID: fitID}
	var total float64
	for typeID, qty := range qtyByType {
		line := shoppingLine{
			TypeID:   typeID,
			Name:     app.deps.TypeNameOrID(ctx, typeID),
			URL:      fmt.Sprintf("/items/type/%d/", typeID),
			Quantity: qty,
		}
		if p, ok := fitview.FitUnitPrice(typicals, trueValues, buildCosts, typeID); ok {
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
	keys := make([]module.SparkKey, len(view.Lines))
	for i, l := range view.Lines {
		keys[i] = module.SparkKey{RegionID: module.TheForgeRegionID, TypeID: l.TypeID}
	}
	if cards := app.deps.PriceSparks(ctx, keys); len(cards) > 0 {
		for i := range view.Lines {
			if view.Lines[i].PriceKnown {
				view.Lines[i].Spark = cards[keys[i]]
			}
		}
	}

	data.Shopping = view
	app.deps.Render(ctx, w, http.StatusOK, "fit_shopping.html", data)
}

// isk formats a float as a template-ready price string.
func isk(f float64) string {
	return esi.FormatISK(f) + " ISK"
}
