package restock

import (
	"context"
	"evesynapse/internal/app/module"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

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
	// Spark is the unit price's hover card: The Forge's stored
	// 30-day line, the region the unit price reads.
	Spark template.HTML
	// Raw values for sorting.
	shortfallRaw int64
	costRaw      float64
}

type restockView struct {
	Rows      []restockRow
	HasData   bool
	TotalCost string
	Short     int // targets with a shortfall
}

func (app *page) handleRestock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := &PageData{ShellData: app.deps.PageShell(ctx)}
	data.Restock = app.buildRestockView(ctx, r)
	if v := data.Restock; len(v.Rows) > 0 {
		keys := make([]module.SparkKey, len(v.Rows))
		for i, row := range v.Rows {
			keys[i] = module.SparkKey{RegionID: module.TheForgeRegionID, TypeID: row.TypeID}
		}
		if cards := app.deps.PriceSparks(ctx, keys); len(cards) > 0 {
			for i := range v.Rows {
				v.Rows[i].Spark = cards[keys[i]]
			}
		}
	}
	app.deps.Render(ctx, w, http.StatusOK, "market_restock.html", data)
}

func (app *page) buildRestockView(ctx context.Context, r *http.Request) *restockView {
	view := &restockView{}
	userID := app.deps.UserID(ctx)
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

	// The house price order: the swept typical sell (The Forge's
	// book), then the stored true-value estimate; never CCP's
	// guide average.
	typical := app.deps.TypicalSells(ctx)
	estimates := app.deps.TrueValues(ctx)

	var totalCost float64
	for _, t := range targets {
		row := restockRow{
			TypeID:    t.TypeID,
			Name:      app.deps.TypeNameOrID(ctx, t.TypeID),
			URL:       "/items/type/" + strconv.FormatInt(t.TypeID, 10) + "/",
			TargetQty: esi.FormatInt(t.TargetQty),
		}
		listed := listedByType[t.TypeID]
		row.ListedQty = esi.FormatInt(listed)
		shortfall := max(t.TargetQty-listed, 0)
		row.Shortfall = esi.FormatInt(shortfall)
		row.shortfallRaw = shortfall

		unit := typical[t.TypeID]
		if unit <= 0 {
			unit = estimates[t.TypeID]
		}
		if unit > 0 {
			row.PriceKnown = true
			row.UnitPrice = esi.FormatISK(unit)
			cost := unit * float64(shortfall)
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
	for _, row := range view.Rows {
		if row.shortfallRaw > 0 {
			view.Short++
		}
	}
	return view
}

// restockListedQty sums open sell order volume_remain per type across
// the user's characters.
func (app *page) restockListedQty(ctx context.Context, r *http.Request) map[int64]int64 {
	out := make(map[int64]int64)
	_, _, links, err := app.deps.PickCharacter(ctx, r, "/market/restock/")
	if err != nil || links == nil {
		return out
	}
	for _, link := range links {
		var orders esi.CharOrders
		if !app.deps.LoadCorpSnapshot(ctx, link.ID, esi.SnapOrders, &orders) {
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

func (app *page) handleRestockSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
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
	if typeID <= 0 {
		typeID = app.restockTypeByName(ctx, r.Form.Get("item"))
	}
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

// restockTypeByName resolves the add form's item box when no pick
// filled the type (no script, or a name typed in full): a number is
// a type id, a name takes its exact match, else the first suggestion.
func (app *page) restockTypeByName(ctx context.Context, raw string) int64 {
	raw = strings.TrimSpace(raw)
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return id
	}
	if len(raw) < 2 {
		return 0
	}
	found, err := app.queries.SuggestSDETypes(ctx, raw)
	if err != nil || len(found) == 0 {
		return 0
	}
	for _, f := range found {
		if strings.EqualFold(f.Name, raw) {
			return f.TypeID
		}
	}
	return found[0].TypeID
}

func (app *page) handleRestockDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := app.deps.UserID(ctx)
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
