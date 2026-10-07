package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
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

	// "Judge as" scoping (character or tag), empty/nil when the
	// plan is judged generally: a skill check against the
	// scope's trained skills and a worth-making verdict priced
	// from the same stored sources as the rest of the page.
	JudgeOptions []judgeOption
	SkillsJudge  *skillsJudge
	Verdict      *planVerdict
}

// judgeOption is one entry of the "Judge as" select.
type judgeOption struct {
	Value    string // "" | "char:<id>" | "tag:<name>"
	Label    string
	Selected bool
}

// skillJudgeLine is one required skill held up against a scope.
type skillJudgeLine struct {
	Text string
	OK   bool
}

// skillsJudge is the can-this-scope-build-it answer.
type skillsJudge struct {
	Heading  string // "Can Burzrujat build this?"
	Known    bool   // the scope has skill snapshots at all
	Note     string // why not, or partial-coverage honesty
	CanBuild bool
	Lines    []skillJudgeLine
	Summary  string
}

// planVerdict is the worth-making economics of a scoped plan.
type planVerdict struct {
	Heading      string // "Worth it? — judged as …"
	Cost         string // net buy cost after stock, "" when unpriceable
	SellValue    string // expected value of the output, "" when unknown
	Profit       string // whole plan, "" unless both sides known
	ProfitPerRun string
	MarginPct    string
	Incomplete   bool // some input price is missing
	Note         string
}

// planRowView is one display row of the plan tree (or, with fewer
// cells filled, of the shopping list).
type planRowView struct {
	Pad        int // indent in px (depth × 18)
	Name       string
	URL        string // item details page
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
	BvB        string // v0.3.33: buy-vs-build delta ("Build saves 1.2M" / "Buy saves 500K")
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
		logging.Errorf("planner: count blueprints: %v", err)
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
			logging.Errorf("planner: search %q: %v", view.Query, err)
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

	if wantCSV(r) && view.Plan != nil {
		servePlannerCSV(w, view.Plan)
		return
	}
	app.render(ctx, w, http.StatusOK, "planner.html", data)
}

// servePlannerCSV writes the build plan's material rows and
// shopping list as a CSV download.
func servePlannerCSV(w http.ResponseWriter, plan *planView) {
	header := []string{"Item", "Per run", "Need", "Have", "To buy", "Build", "ME/TE", "Time", "Unit price", "Buy cost", "Buy vs build"}
	rows := make([][]string, 0, len(plan.Rows)+len(plan.Shopping)+2)
	for _, row := range plan.Rows {
		rows = append(rows, []string{
			row.Name,
			row.Base,
			row.Need,
			row.Have,
			row.ToBuy,
			row.RunsLine,
			row.ME,
			row.Time,
			row.UnitPrice,
			row.LineCost,
			row.BvB,
		})
	}
	if len(plan.Shopping) > 0 {
		rows = append(rows, []string{}) // blank separator
		rows = append(rows, []string{"Shopping list", "Qty", "Unit price", "Line cost"})
		for _, row := range plan.Shopping {
			rows = append(rows, []string{
				row.Name,
				row.Need,
				row.UnitPrice,
				row.LineCost,
			})
		}
	}
	serveCSV(w, "build-plan", header, rows)
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

	// "Judge as" scoping: the account's characters are the
	// universe of scopes (one character, or everyone carrying a
	// tag). An unknown or stale pick degrades to the general
	// view, exactly like a stale widget scope on Home.
	userChars := app.plannerAccountChars(ctx)
	scope, judgeOptions := resolveJudgeScope(q.Get("judge"), userChars)
	inputChars := userChars
	if scope.active() {
		inputChars = scope.Chars
	}
	scoped := app.plannerInputsForChars(ctx, inputChars)
	owned, stock := scoped.Owned, scoped.Stock
	prices := app.valuationPrices(ctx)

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
		logging.Errorf("planner: build plan for product %d: %v", productID, err)
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
	if scope.active() {
		view.StockNote = scopeStockNote(scope, scoped)
	} else if len(stock) > 0 {
		view.StockNote = "Requirements are net of everything your linked characters currently hold."
	}
	view.JudgeOptions = judgeOptions
	if scope.active() {
		view.SkillsJudge = judgeSkills(scope, scoped, requiredSkillLevels(root), nameOf)
		view.Verdict = planVerdictFor(res, prices, productID, scope.Label)
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
			URL:  fmt.Sprintf("/items/type/%d/", line.TypeID),
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
		view.PricesNote = fmt.Sprintf("Unit prices are each item's average market price across the cluster; %d shopping item(s) have no price data, so totals are partial.", missing)
	} else {
		view.PricesNote = "Unit prices are each item's average market price across the cluster (adjusted price where no average exists)."
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
		URL:  fmt.Sprintf("/items/type/%d/", n.TypeID),
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
		// v0.3.33: per-node buy-vs-build delta.
		if n.BvBApplicable && n.PriceKnown {
			if n.BvBDelta > 0 {
				row.BvB = "Build saves " + isk(n.BvBDelta)
			} else if n.BvBDelta < 0 {
				row.BvB = "Buy saves " + isk(-n.BvBDelta)
			} else {
				row.BvB = "Even"
			}
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
			logging.Errorf("planner: blueprint for product %d: %v", productTypeID, err)
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
		logging.Errorf("planner: materials for blueprint %d: %v", row.BlueprintTypeID, err)
	}
	if skills, err := s.app.queries.ListSDEBlueprintSkills(s.ctx, row.BlueprintTypeID); err == nil {
		for _, sk := range skills {
			bp.Skills = append(bp.Skills, plannerSkillReq{TypeID: sk.SkillTypeID, Level: sk.Level})
		}
	} else {
		logging.Errorf("planner: skills for blueprint %d: %v", row.BlueprintTypeID, err)
	}
	s.memo[productTypeID] = bp
	return bp, true
}

// plannerAccountChars lists the signed-in user's characters —
// the universe the planner's scopes pick from. A dev-login
// session (no user) simply has none.
func (app *Application) plannerAccountChars(ctx context.Context) []db.Character {
	userID := int64(app.sessions.GetInt(ctx, sessionUserID))
	if userID == 0 {
		return nil
	}
	chars, err := app.queries.ListCharactersByUser(ctx, userID)
	if err != nil {
		logging.Errorf("planner: list characters for user %d: %v", userID, err)
		return nil
	}
	return chars
}

// plannerScopeData is everything a set of characters contributes
// to a plan: their owned blueprints and stockpile (for netting),
// how many of them have hangar data at all (honesty), and each
// character's trained skills where a snapshot exists.
type plannerScopeData struct {
	Owned        map[int64]ownedBlueprint
	Stock        map[int64]int64
	AssetsLoaded int // characters whose assets snapshot was present
	Skills       map[int64]map[int64]int
}

// plannerInputsForChars aggregates owned blueprints (best copy
// per blueprint type), total stockpile, and trained skills over
// exactly the given characters — the whole account for the
// general view, one character or one tag's members when judged.
func (app *Application) plannerInputsForChars(ctx context.Context, chars []db.Character) plannerScopeData {
	data := plannerScopeData{
		Owned:  make(map[int64]ownedBlueprint),
		Stock:  make(map[int64]int64),
		Skills: make(map[int64]map[int64]int),
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

	for _, ch := range chars {
		var bps esi.Blueprints
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapBlueprints, &bps) {
			for _, bp := range bps {
				consider(bp)
			}
		}
		var assets []esi.Asset
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapAssets, &assets) {
			data.AssetsLoaded++
			for _, a := range assets {
				if a.Quantity > 0 {
					data.Stock[a.TypeID] += a.Quantity
				}
			}
		}
		var skills esi.Skills
		if app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapSkills, &skills) {
			trained := make(map[int64]int, len(skills.Skills))
			for _, s := range skills.Skills {
				trained[s.SkillID] = s.TrainedSkillLevel
			}
			data.Skills[ch.CharacterID] = trained
		}
	}
	for typeID, b := range best {
		data.Owned[typeID] = ownedBlueprint{ME: b.me, TE: b.te}
	}
	return data
}

// judgeScope is a resolved "Judge as" pick: the characters it
// covers and how the copy names it.
type judgeScope struct {
	Kind  string // "" (general) | "char" | "tag"
	Tag   string
	Label string // "Burzrujat" | `the "industry" tag`
	Chars []db.Character
}

func (s judgeScope) active() bool { return s.Kind != "" && len(s.Chars) > 0 }

// resolveJudgeScope validates a raw `judge` parameter against
// the account and builds the select's options with the current
// pick marked. Anything stale or foreign degrades to the
// general view — never an empty judgment.
func resolveJudgeScope(raw string, chars []db.Character) (judgeScope, []judgeOption) {
	scope := judgeScope{}
	selected := ""
	switch {
	case strings.HasPrefix(raw, "char:"):
		if id, err := strconv.ParseInt(strings.TrimPrefix(raw, "char:"), 10, 64); err == nil && id > 0 {
			for _, ch := range chars {
				if ch.CharacterID == id {
					scope = judgeScope{Kind: "char", Label: ch.Name, Chars: []db.Character{ch}}
					selected = raw
				}
			}
		}
	case strings.HasPrefix(raw, "tag:"):
		tag := strings.TrimPrefix(raw, "tag:")
		var members []db.Character
		for _, ch := range chars {
			for _, t := range splitTags(ch.Tags) {
				if t == tag {
					members = append(members, ch)
					break
				}
			}
		}
		if tag != "" && len(members) > 0 {
			scope = judgeScope{Kind: "tag", Tag: tag, Label: fmt.Sprintf("the %q tag", tag), Chars: members}
			selected = raw
		}
	}

	options := []judgeOption{{Value: "", Label: "Everyone together"}}
	for _, ch := range chars {
		value := "char:" + strconv.FormatInt(ch.CharacterID, 10)
		options = append(options, judgeOption{Value: value, Label: ch.Name, Selected: value == selected})
	}
	for _, tag := range userTags(chars) {
		value := "tag:" + tag
		options = append(options, judgeOption{Value: value, Label: "Tag: " + tag, Selected: value == selected})
	}
	if len(chars) == 0 {
		return judgeScope{}, nil
	}
	return scope, options
}

// scopeStockNote says whose hangars the plan netted against,
// including the honest version when the data isn't in yet.
func scopeStockNote(scope judgeScope, data plannerScopeData) string {
	switch scope.Kind {
	case "char":
		name := scope.Chars[0].Name
		if data.AssetsLoaded > 0 {
			return fmt.Sprintf("Requirements are netted against what %s holds.", name)
		}
		return fmt.Sprintf("%s's hangars haven't synced yet — everything counts as to-buy for now.", name)
	case "tag":
		n := len(scope.Chars)
		if data.AssetsLoaded == 0 {
			return fmt.Sprintf("None of your %q characters have hangar data yet — everything counts as to-buy for now.", scope.Tag)
		}
		note := fmt.Sprintf("Requirements are netted against the combined hangars of your %q characters (%d of them).", scope.Tag, n)
		if data.AssetsLoaded < n {
			note += fmt.Sprintf(" Hangar data is in for %d of %d so far.", data.AssetsLoaded, n)
		}
		return note
	}
	return ""
}

// requiredSkillLevels collects the highest required level per
// skill across every blueprint the plan actually builds.
func requiredSkillLevels(root *planNode) map[int64]int64 {
	reqs := make(map[int64]int64)
	walkPlan(root, func(n *planNode) {
		if n.Blueprint == nil {
			return
		}
		for _, s := range n.Blueprint.Skills {
			if s.Level > reqs[s.TypeID] {
				reqs[s.TypeID] = s.Level
			}
		}
	})
	return reqs
}

// judgeSkills holds a plan's skill requirements up against a
// scope: one character's trained skills, or the best level
// across a tag's characters. Levels are EVE's 0–5.
func judgeSkills(scope judgeScope, data plannerScopeData, reqs map[int64]int64, nameOf func(int64) string) *skillsJudge {
	judge := &skillsJudge{}
	switch scope.Kind {
	case "char":
		judge.Heading = fmt.Sprintf("Can %s build this?", scope.Chars[0].Name)
	case "tag":
		judge.Heading = fmt.Sprintf("Can your %q characters build this?", scope.Tag)
	}

	loaded := 0
	for _, ch := range scope.Chars {
		if _, ok := data.Skills[ch.CharacterID]; ok {
			loaded++
		}
	}
	if loaded == 0 {
		switch scope.Kind {
		case "char":
			judge.Note = fmt.Sprintf("Skills haven't synced for %s yet — once they land, this says what they can build.", scope.Chars[0].Name)
		default:
			judge.Note = "Skills haven't synced for anyone with this tag yet — once they land, this says what they can build."
		}
		return judge
	}
	judge.Known = true
	if scope.Kind == "tag" && loaded < len(scope.Chars) {
		judge.Note = fmt.Sprintf("Skills are on file for %d of %d tagged characters; the rest count as untrained until they sync.", loaded, len(scope.Chars))
	}

	best := func(skillID int64) int {
		level := 0
		for _, ch := range scope.Chars {
			if trained, ok := data.Skills[ch.CharacterID]; ok && trained[skillID] > level {
				level = trained[skillID]
			}
		}
		return level
	}

	ids := make([]int64, 0, len(reqs))
	for id := range reqs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return nameOf(ids[i]) < nameOf(ids[j]) })

	judge.CanBuild = true
	for _, id := range ids {
		need, have := reqs[id], int64(best(id))
		ok := have >= need
		if !ok {
			judge.CanBuild = false
		}
		line := skillJudgeLine{OK: ok}
		switch {
		case scope.Kind == "tag" && have == 0:
			line.Text = fmt.Sprintf("%s — needs %s · nobody with this tag has it trained", nameOf(id), esi.RomanLevel(int(need)))
		case scope.Kind == "tag":
			line.Text = fmt.Sprintf("%s — needs %s · best on the tag %s", nameOf(id), esi.RomanLevel(int(need)), esi.RomanLevel(int(have)))
		case have == 0:
			line.Text = fmt.Sprintf("%s — needs %s · not trained", nameOf(id), esi.RomanLevel(int(need)))
		default:
			line.Text = fmt.Sprintf("%s — needs %s · trained %s", nameOf(id), esi.RomanLevel(int(need)), esi.RomanLevel(int(have)))
		}
		judge.Lines = append(judge.Lines, line)
	}

	switch {
	case len(reqs) == 0:
		judge.Summary = "Nothing in this build chain lists a skill requirement."
	case judge.CanBuild:
		judge.Summary = "Every skill in the chain is covered."
	default:
		judge.Summary = "Not yet — the short skills are listed above."
	}
	return judge
}

// planVerdictFor prices a scoped plan's judgement: the net buy
// cost after stock against the expected sell value of what
// comes out. Unknown prices stay unknown — the verdict says so
// instead of pricing missing inputs at zero.
func planVerdictFor(res *planResult, prices map[int64]esi.MarketPrice, productID int64, label string) *planVerdict {
	root := res.Root
	verdict := &planVerdict{Heading: "Worth making — judged as " + label}
	var notes []string

	costKnown := root.CostComplete && res.UnpricedLines == 0
	if costKnown {
		verdict.Cost = isk(root.LineCost)
	} else if res.UnpricedLines > 0 {
		verdict.Incomplete = true
		notes = append(notes, fmt.Sprintf("Prices incomplete — %d shopping items have no price yet, so a profit figure would be a guess.", res.UnpricedLines))
	}

	if p, ok := unitPrice(prices, productID); ok {
		value := p * float64(root.ProducedQty)
		verdict.SellValue = isk(value)
		if costKnown && value > 0 {
			profit := value - root.LineCost
			verdict.Profit = isk(profit)
			if root.Runs > 0 {
				verdict.ProfitPerRun = isk(profit / float64(root.Runs))
			}
			verdict.MarginPct = fmt.Sprintf("%.1f%% of the sell value", profit/value*100)
		}
	} else {
		notes = append(notes, "No market price for the finished item yet, so there's no sell value to judge against.")
	}
	verdict.Note = strings.Join(notes, " ")
	return verdict
}
