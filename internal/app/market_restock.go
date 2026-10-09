package app

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

// ---------------------------------------------------------------------------
// Restock planner.
//
// Target stock levels per item. For each target the page shows what's
// currently listed (open sell orders), the shortfall against target,
// and the estimated restock cost at market sell prices. min_margin_pct
// is stored for future margin-threshold alerts (not yet enforced).
// ---------------------------------------------------------------------------

type restockRow struct {
	TypeID     int64
	Name       string
	URL        string
	TargetQty  string
	ListedQty  string
	Shortfall  string
	UnitPrice  string
	EstCost    string
	PriceKnown bool
	// Raw values for sorting.
	shortfallRaw int64
	costRaw      float64
}

type restockView struct {
	Rows      []restockRow
	HasData   bool
	TotalCost string
}

func (app *Application) handleRestock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)
	data.Restock = app.buildRestockView(ctx, r)
	app.render(ctx, w, http.StatusOK, "market_restock.html", data)
}

func (app *Application) buildRestockView(ctx context.Context, r *http.Request) *restockView {
	view := &restockView{}
	userID := app.userID(ctx)
	if userID == 0 {
		return view
	}

	targets, err := app.queries.ListRestockTargets(ctx, userID)
	if err != nil {
		logging.Errorf("restock: list targets for user %d: %v", userID, err)
		return view
	}
	if len(targets) == 0 {
		return view
	}

	// Gather open sell orders across the user's characters to
	// compute listed quantities per type.
	listedByType := app.restockListedQty(ctx, r)

	// Market sell prices from the guide cache.
	prices, _ := app.marketPrices(ctx)

	var totalCost float64
	for _, t := range targets {
		row := restockRow{
			TypeID:    t.TypeID,
			Name:      app.typeNameOrID(ctx, t.TypeID),
			URL:       "/items/type/" + strconv.FormatInt(t.TypeID, 10) + "/",
			TargetQty: esi.FormatInt(t.TargetQty),
		}
		listed := listedByType[t.TypeID]
		row.ListedQty = esi.FormatInt(listed)
		shortfall := t.TargetQty - listed
		if shortfall < 0 {
			shortfall = 0
		}
		row.Shortfall = esi.FormatInt(shortfall)
		row.shortfallRaw = shortfall

		if mp, ok := prices[t.TypeID]; ok && mp.AveragePrice > 0 {
			row.PriceKnown = true
			row.UnitPrice = esi.FormatISK(mp.AveragePrice)
			cost := mp.AveragePrice * float64(shortfall)
			row.EstCost = esi.FormatISK(cost)
			row.costRaw = cost
			totalCost += cost
		} else {
			row.UnitPrice = "no price data"
			row.EstCost = "—"
		}
		view.Rows = append(view.Rows, row)
	}

	// Sort by shortfall descending (most urgent first), then by cost.
	sort.Slice(view.Rows, func(i, j int) bool {
		if view.Rows[i].shortfallRaw != view.Rows[j].shortfallRaw {
			return view.Rows[i].shortfallRaw > view.Rows[j].shortfallRaw
		}
		return view.Rows[i].costRaw > view.Rows[j].costRaw
	})

	view.HasData = true
	view.TotalCost = esi.FormatISK(totalCost)
	return view
}

// restockListedQty sums open sell order volume_remain per type across
// the user's characters.
func (app *Application) restockListedQty(ctx context.Context, r *http.Request) map[int64]int64 {
	out := make(map[int64]int64)
	_, _, links, err := app.pickCharacter(ctx, r, "/market/restock/")
	if err != nil || links == nil {
		return out
	}
	for _, link := range links {
		var orders esi.CharOrders
		if !app.econSection(ctx, link.ID, esi.SnapOrders, &orders).Loaded {
			continue
		}
		for _, o := range orders {
			if o.IsBuyOrder {
				continue
			}
			out[o.TypeID] += o.VolumeRemain
		}
	}
	return out
}

func (app *Application) handleRestockSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		logging.Errorf("restock: parse save form: %v", err)
		http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
		return
	}
	typeID, _ := strconv.ParseInt(r.Form.Get("type_id"), 10, 64)
	targetQty, _ := strconv.ParseInt(r.Form.Get("target_qty"), 10, 64)
	marginPct, _ := strconv.ParseFloat(r.Form.Get("min_margin_pct"), 64)
	if typeID <= 0 || targetQty < 0 {
		http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
		return
	}
	if err := app.queries.UpsertRestockTarget(ctx, db.UpsertRestockTargetParams{
		UserID:       userID,
		TypeID:       typeID,
		TargetQty:    targetQty,
		MinMarginPct: marginPct,
	}); err != nil {
		logging.Errorf("restock: upsert target user %d type %d: %v", userID, typeID, err)
	}
	http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
}

func (app *Application) handleRestockDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.userID(ctx)
	if userID == 0 {
		http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
		return
	}
	typeID, _ := strconv.ParseInt(r.Form.Get("type_id"), 10, 64)
	if typeID > 0 {
		if err := app.queries.DeleteRestockTarget(ctx, db.DeleteRestockTargetParams{
			UserID: userID,
			TypeID: typeID,
		}); err != nil {
			logging.Errorf("restock: delete target user %d type %d: %v", userID, typeID, err)
		}
	}
	http.Redirect(w, r, "/market/restock/", http.StatusSeeOther)
}
