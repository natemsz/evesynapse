package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Build Planner page (/planner/): pick a manufacturable product,
// get its full build chain with owned-blueprint ME/TE prefilled,
// your stockpile netted against requirements, and a priced
// shopping list. Everything reads local state — the SDE industry
// tables, the blueprint/asset snapshots, and the price cache — so
// the handler never touches ESI.
// ---------------------------------------------------------------------------

// plannerView is the Planner page body.
type plannerView struct {
	Warming  bool // planner tables empty: SDE industry import outstanding
	Query    string
	Searched bool
	Results  []plannerResultRow
	Notice   string // soft dead-end (pick can't be manufactured, plan too large)
	Plan     *planView
}

// plannerResultRow is one product search hit.
type plannerResultRow struct {
	Name string
	URL  string // /planner/?product=<type id>
}

// planView is one computed build plan, display-ready.
type planView struct {
	ProductTypeID int64
	ProductName   string
	BlueprintName string
	Runs          int64
	ProducedLine  string // "1,200 units (12 runs × 100 per run)"
	StockNote     string // "Requirements are netted against everything your characters hold."
	MaxRunNote    string // blueprint max production limit, "" when unknown
	Skills        []string
	Rows          []planRowView
	MEInputs      []planMEInput
	Shopping      []planRowView // reuses the row shape; only name/qty/price cells fill
	ShoppingTotal string        // "" when some lines are unpriced
	TotalTime     string
	ProductValue  string
	Margin        string
	MarginNote    string
	PricesNote    string
}

// planRowView is one display row of the plan tree (or, with fewer
// cells filled, of the shopping list).
type planRowView struct {
	Pad        int // indent in px (depth × 18)
	Name       string
	URL        string // market item view
	Badge      string // "Build" | "Buy" | "Have"
	Base       string // "×86 per run"
	Need       string
	Have       string
	ToBuy      string
	RunsLine   string // "3 runs → 300 produced"
	ME         string // "10% (owned blueprint)"
	Time       string
	UnitPrice  string
	LineCost   string
	Note       string // surplus / cycle / covered-by-stock notes
	SkillsLine string
}

// planMEInput is one blueprint's ME override field in the plan form.
type planMEInput struct {
	FieldName string // me_<blueprint type id>
	Label     string // product built from this blueprint
	Value     int
}

// plannerMaxRunsInput clamps the runs form field; the node cap in
// the engine is the real guard against runaway plans.
const plannerMaxRunsInput = 1000000

func (app *Application) handlePlanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := pageData{
		LoggedIn:      true,
		CharacterName: app.sessions.GetString(ctx, sessionCharacterName),
		SSOConfigured: app.cfg.SSOConfigured(),
	}
	view := &plannerView{}
	data.Planner = view

	count, err := app.queries.CountSDEBlueprints(ctx)
	if err != nil {
		log.Printf("planner: count blueprints: %v", err)
		data.Error = "Could not load planner data; check the server log."
		app.render(ctx, w, http.StatusOK, "planner.html", data)
		return
	}
	if count == 0 {
		view.Warming = true
		app.render(ctx, w, http.StatusOK, "planner.html", data)
		return
	}

	q := r.URL.Query()
	view.Query = strings.TrimSpace(q.Get("q"))
	if view.Query != "" {
		view.Searched = true
		hits, err := app.queries.SearchManufacturableProducts(ctx, view.Query)
		if err != nil {
			log.Printf("planner: search %q: %v", view.Query, err)
			data.Error = "Search failed; check the server log."
		}
		for _, hit := range hits {
			view.Results = append(view.Results, plannerResultRow{
				Name: hit.Name,
				URL:  fmt.Sprintf("/planner/?product=%d", hit.TypeID),
			})
		}
	}

	if productID, perr := strconv.ParseInt(q.Get("product"), 10, 64); perr == nil && productID > 0 {
		plan, notice := app.buildPlanView(ctx, q, productID)
		switch {
		case notice != "":
			view.Notice = notice
		case plan != nil:
			view.Plan = plan
		}
	}

	app.render(ctx, w, http.StatusOK, "planner.html", data)
}

// buildPlanView runs the engine for one product pick and shapes
// the result for the template. notice is a user-safe dead-end.
func (app *Application) buildPlanView(ctx context.Context, q url.Values, productID int64) (*planView, string) {
	runs := int64(1)
	if raw := q.Get("runs"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 1 {
			runs = n
		}
	}
	if runs > plannerMaxRunsInput {
		runs = plannerMaxRunsInput
	}
	overrides := map[int64]int{}
	for key, vals := range q {
		if !strings.HasPrefix(key, "me_") || len(vals) == 0 {
			continue
		}
		bpID, err1 := strconv.ParseInt(strings.TrimPrefix(key, "me_"), 10, 64)
		me, err2 := strconv.Atoi(vals[0])
		if err1 == nil && err2 == nil && bpID > 0 && me >= 0 && me <= 10 {
			overrides[bpID] = me
		}
	}

	src := &sdePlannerSource{app: app, ctx: ctx, memo: make(map[int64]*plannerBlueprint), none: make(map[int64]bool)}
	owned, stock := app.plannerUserInputs(ctx)
	prices := app.cachedPrices()

	res, err := buildPlan(src, productID, plannerInput{
		Runs:       runs,
		MEOverride: overrides,
		Owned:      owned,
		Stock:      stock,
		Prices:     prices,
	})
	if errors.Is(err, errPlannerNotManufacturable) {
		return nil, "That item has no manufacturing blueprint in the local data, so it can't be planned."
	}
	if err != nil {
		log.Printf("planner: build plan for product %d: %v", productID, err)
		return nil, "That plan couldn't be computed; check the server log."
	}
	if res.TooLarge {
		return nil, "That plan is too large to lay out — try fewer runs."
	}

	// Names for everything on screen: tree items, shopping items,
	// blueprints, skills.
	ids := []int64{productID}
	walkPlan(res.Root, func(n *planNode) {
		ids = append(ids, n.TypeID)
		if n.Blueprint != nil {
			ids = append(ids, n.Blueprint.BlueprintTypeID)
			for _, s := range n.Blueprint.Skills {
				ids = append(ids, s.TypeID)
			}
		}
	})
	names := app.esi.CachedTypeNames(ctx, ids)
	nameOf := func(id int64) string {
		if n, ok := names[id]; ok && n != "" {
			return n
		}
		return fmt.Sprintf("Type #%d", id)
	}

	root := res.Root
	rootBP := root.Blueprint
	view := &planView{
		ProductTypeID: productID,
		ProductName:   nameOf(productID),
		Runs:          runs,
	}
	if rootBP != nil {
		view.BlueprintName = nameOf(rootBP.BlueprintTypeID)
		perRun := quantityPerRun(rootBP)
		if perRun > 1 {
			view.ProducedLine = fmt.Sprintf("%s units (%s runs × %s per run)",
				esi.FormatInt(root.ProducedQty), esi.FormatInt(root.Runs), esi.FormatInt(perRun))
		} else {
			view.ProducedLine = fmt.Sprintf("%s units (%s runs)", esi.FormatInt(root.ProducedQty), esi.FormatInt(root.Runs))
		}
		if rootBP.MaxProductionLimit > 0 {
			view.MaxRunNote = fmt.Sprintf("Blueprint max production limit: %s runs", esi.FormatInt(rootBP.MaxProductionLimit))
		}
		for _, s := range rootBP.Skills {
			view.Skills = append(view.Skills, fmt.Sprintf("%s %s", nameOf(s.TypeID), esi.RomanLevel(int(s.Level))))
		}
	}
	if len(stock) > 0 {
		view.StockNote = "Requirements are net of everything your linked characters currently hold."
	}

	// Flatten the tree; collect the ME inputs once per blueprint.
	seenME := make(map[int64]bool)
	walkPlan(root, func(n *planNode) {
		view.Rows = append(view.Rows, planRow(n, nameOf))
		if n.Blueprint != nil && !seenME[n.Blueprint.BlueprintTypeID] {
			seenME[n.Blueprint.BlueprintTypeID] = true
			view.MEInputs = append(view.MEInputs, planMEInput{
				FieldName: fmt.Sprintf("me_%d", n.Blueprint.BlueprintTypeID),
				Label:     nameOf(n.TypeID),
				Value:     n.ME,
			})
		}
	})
	sort.Slice(view.MEInputs, func(i, j int) bool { return view.MEInputs[i].Label < view.MEInputs[j].Label })

	for _, line := range res.Shopping {
		row := planRowView{
			Name: nameOf(line.TypeID),
			URL:  fmt.Sprintf("/market/?type=%d", line.TypeID),
			Need: esi.FormatInt(line.Quantity),
		}
		if line.PriceKnown {
			row.UnitPrice = isk(line.UnitPrice)
			row.LineCost = isk(line.LineCost)
		} else {
			row.UnitPrice = "no price data"
		}
		view.Shopping = append(view.Shopping, row)
	}
	sort.Slice(view.Shopping, func(i, j int) bool { return view.Shopping[i].Name < view.Shopping[j].Name })

	view.TotalTime = humanDuration(time.Duration(res.TotalSeconds) * time.Second)

	buyTotal := root.LineCost
	if !root.CostComplete || res.UnpricedLines > 0 {
		missing := res.UnpricedLines
		view.PricesNote = fmt.Sprintf("Prices come from the market guide cache; %d shopping item(s) have no price data, so totals are partial.", missing)
	} else {
		view.PricesNote = "Prices come from the market guide cache (average, adjusted where no average exists)."
		view.ShoppingTotal = isk(buyTotal)
	}

	if p, ok := unitPrice(prices, productID); ok {
		value := p * float64(root.ProducedQty)
		view.ProductValue = isk(value)
		if root.CostComplete && res.UnpricedLines == 0 && value > 0 {
			margin := value - buyTotal
			view.Margin = isk(margin)
			view.MarginNote = fmt.Sprintf("%.1f%% of market value", margin/value*100)
		} else if root.CostComplete && res.UnpricedLines == 0 && value <= 0 {
			view.MarginNote = "No margin to compute at a zero market value."
		} else {
			view.MarginNote = "Margin needs a full buy cost; some prices are missing."
		}
	} else {
		view.MarginNote = "No market price for the finished product yet, so no margin to show."
	}

	return view, ""
}

// isk formats a float as a template-ready price string.
func isk(f float64) string {
	return esi.FormatISK(f) + " ISK"
}

// planRow shapes one engine node for the tree table.
func planRow(n *planNode, nameOf func(int64) string) planRowView {
	row := planRowView{
		Pad:  n.Depth * 18,
		Name: nameOf(n.TypeID),
		URL:  fmt.Sprintf("/market/?type=%d", n.TypeID),
		Need: esi.FormatInt(n.RequiredQty),
	}
	if n.BasePerRun > 0 {
		row.Base = "×" + esi.FormatInt(n.BasePerRun)
	}
	if n.Have > 0 {
		row.Have = esi.FormatInt(n.Have)
	}
	switch {
	case n.Blueprint != nil:
		row.Badge = "Build"
		row.RunsLine = fmt.Sprintf("%s runs → %s", esi.FormatInt(n.Runs), esi.FormatInt(n.ProducedQty))
		switch n.MESource {
		case "owned":
			row.ME = fmt.Sprintf("ME %d%% · TE %d%% (owned blueprint)", n.ME, n.TE)
		case "override":
			row.ME = fmt.Sprintf("ME %d%% · TE %d%% (your override)", n.ME, n.TE)
		default:
			row.ME = fmt.Sprintf("ME %d%% (assumed unresearched)", n.ME)
		}
		row.Time = humanDuration(time.Duration(n.TimeSeconds) * time.Second)
		if len(n.Blueprint.Skills) > 0 {
			parts := make([]string, 0, len(n.Blueprint.Skills))
			for _, s := range n.Blueprint.Skills {
				parts = append(parts, fmt.Sprintf("%s %s", nameOf(s.TypeID), esi.RomanLevel(int(s.Level))))
			}
			row.SkillsLine = "Requires " + strings.Join(parts, ", ")
		}
		var notes []string
		if n.SurplusQty > 0 {
			notes = append(notes, fmt.Sprintf("%s left over", esi.FormatInt(n.SurplusQty)))
		}
		if n.Shortfall <= 0 {
			notes = append(notes, "covered by stock on hand")
		}
		row.Note = strings.Join(notes, " · ")
		if n.CostComplete {
			row.LineCost = isk(n.LineCost)
		} else {
			row.LineCost = "partial"
		}
	case n.Shortfall <= 0:
		row.Badge = "Have"
		row.Note = "covered by stock on hand"
	default:
		row.Badge = "Buy"
		row.ToBuy = esi.FormatInt(n.Shortfall)
		if n.PriceKnown {
			row.UnitPrice = isk(n.UnitPrice)
			row.LineCost = isk(n.LineCost)
		} else {
			row.UnitPrice = "no price data"
		}
		if n.Cycle {
			row.Note = "part of a build cycle — buy it"
		} else if n.DepthCapped {
			row.Note = "chain cut off here — buy it"
		}
	}
	return row
}

// sdePlannerSource resolves blueprints from the local SDE tables,
// memoized per render (a recipe tree revisits the same products).
type sdePlannerSource struct {
	app  *Application
	ctx  context.Context
	memo map[int64]*plannerBlueprint
	none map[int64]bool
}

func (s *sdePlannerSource) BlueprintForProduct(productTypeID int64) (*plannerBlueprint, bool) {
	if bp, ok := s.memo[productTypeID]; ok {
		return bp, true
	}
	if s.none[productTypeID] {
		return nil, false
	}
	row, err := s.app.queries.GetSDEBlueprintForProduct(s.ctx, productTypeID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("planner: blueprint for product %d: %v", productTypeID, err)
		}
		s.none[productTypeID] = true
		return nil, false
	}
	bp := &plannerBlueprint{
		BlueprintTypeID:    row.BlueprintTypeID,
		ProductTypeID:      row.ProductTypeID,
		ProductQuantity:    row.ProductQuantity,
		MaxProductionLimit: row.MaxProductionLimit,
		TimeSeconds:        row.ManufacturingTimeSeconds,
	}
	if mats, err := s.app.queries.ListSDEBlueprintMaterials(s.ctx, row.BlueprintTypeID); err == nil {
		for _, m := range mats {
			bp.Materials = append(bp.Materials, plannerMaterial{TypeID: m.MaterialTypeID, Quantity: m.Quantity})
		}
	} else {
		log.Printf("planner: materials for blueprint %d: %v", row.BlueprintTypeID, err)
	}
	if skills, err := s.app.queries.ListSDEBlueprintSkills(s.ctx, row.BlueprintTypeID); err == nil {
		for _, sk := range skills {
			bp.Skills = append(bp.Skills, plannerSkillReq{TypeID: sk.SkillTypeID, Level: sk.Level})
		}
	} else {
		log.Printf("planner: skills for blueprint %d: %v", row.BlueprintTypeID, err)
	}
	s.memo[productTypeID] = bp
	return bp, true
}

// plannerUserInputs aggregates the signed-in user's owned
// blueprints (best copy per blueprint type) and total stockpile
// (per item type, across every linked character) from snapshots.
// A dev-login session (no user) and characters without snapshots
// simply contribute nothing.
func (app *Application) plannerUserInputs(ctx context.Context) (owned map[int64]ownedBlueprint, stock map[int64]int64) {
	owned = make(map[int64]ownedBlueprint)
	stock = make(map[int64]int64)

	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		return owned, stock
	}
	characters, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		log.Printf("planner: list characters for user %d: %v", userID, err)
		return owned, stock
	}

	type ownedBest struct {
		original bool
		me, te   int
	}
	best := make(map[int64]ownedBest)
	consider := func(bp esi.Blueprint) {
		cand := ownedBest{
			original: bp.Quantity == -1, // ESI: -1 original, -2 copy
			me:       int(bp.MaterialEfficiency),
			te:       int(bp.TimeEfficiency),
		}
		cur, ok := best[bp.TypeID]
		switch {
		case !ok:
			best[bp.TypeID] = cand
		case cand.original != cur.original:
			if cand.original {
				best[bp.TypeID] = cand
			}
		case cand.me != cur.me:
			if cand.me > cur.me {
				best[bp.TypeID] = cand
			}
		case cand.te > cur.te:
			best[bp.TypeID] = cand
		}
	}

	for _, ch := range characters {
		var bps esi.Blueprints
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapBlueprints, &bps) {
			for _, bp := range bps {
				consider(bp)
			}
		}
		var assets []esi.Asset
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapAssets, &assets) {
			for _, a := range assets {
				if a.Quantity > 0 {
					stock[a.TypeID] += a.Quantity
				}
			}
		}
	}
	for typeID, b := range best {
		owned[typeID] = ownedBlueprint{ME: b.me, TE: b.te}
	}
	return owned, stock
}
