package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"

	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/piplan"
)

// ---------------------------------------------------------------------------
// PI Planner (/industry/pi/): full planetary-interaction simulator
// under the Industry hub, modelled on evepitool.com. Pick a P1–P4
// product, set how much to make, and get the whole chain — raw
// needs, factory counts, extractor colonies, CPU/Power fit,
// POCO taxes and market economics. Prices come from the stored
// market guide (valuationPrices); colonies pre-fill planet types.
//
// The engine (internal/piplan) is pure. This handler resolves
// type IDs for pricing, reads the acting character's colonies
// for the planet pre-fill, and formats for the template.
// ---------------------------------------------------------------------------

type piPlanetCheck struct {
	Name    string
	Checked bool
	Owned   bool
}

type piGraphNode struct {
	Name    string
	Tier    string
	Class   string // selected|input|output|dimmed|covered|missing
	Planets string // planet types yielding it (raws) or ""
	Price   string
}

type piGraphCol struct {
	Title string
	Nodes []piGraphNode
}

type piLineRow struct {
	Name      string
	Tier      string
	Needed    string
	PerHour   string
	Factories string
	Facility  string
	Volume    string
	Price     string
	Value     string
}

type piColonyRow struct {
	Title     string
	Planet    string
	Detail    string
	CPU       string
	Fits      bool
	Breakdown []string
}

type piProfitRow struct {
	Name      string
	URL       string
	Margin    string
	MarginVal float64
	Profit    string
	Class     string
}

type piPlannerView struct {
	Targets        []string
	Target         string
	Amount         int64
	AmountStr      string
	CCLevel        int
	Heads          int
	Yield          int64
	TaxPct         float64
	TaxStr         string
	PlanetChecks   []piPlanetCheck
	OwnedPlanets   []string
	OwnedNote      string
	CoverageText   string
	CoverageClass  string
	MissingText    string
	MinimalText    string
	SingleText     string
	Graph          []piGraphCol
	GraphJSON      string // the graph as JSON, carried on a data attribute (no inline <script>: CSP)
	HasPlan        bool
	PlanTarget     string
	Lines          []piLineRow
	Colonies       []piColonyRow
	Revenue        string
	RawCost        string
	StepCost       string
	ProfitWhole    string
	ProfitWholeCls string
	MarginWhole    string
	ProfitStep     string
	MarginStep     string
	ExportTax      string
	ImportTax      string
	PricesNote     string
	Profitable     []piProfitRow
	FacilityNote   string
}

func (app *Application) handlePIPlanner(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data := app.page(ctx)

	_, active, links, err := app.pickCharacter(ctx, r, "/industry/pi/")
	if err != nil {
		logging.Errorf("piplanner: list characters: %v", err)
		data.Error = "Could not load PI planner data; check the server log."
		app.render(ctx, w, http.StatusOK, "piplanner.html", data)
		return
	}
	// Standalone like evepitool: no character is fine. When one
	// exists, its colonies pre-fill the planet picker.
	var ownedPlanets []string
	ownedNote := ""
	if links != nil && active.CharacterID != 0 {
		data.PIPlannerChars = links
		ownedPlanets, ownedNote = app.piOwnedPlanetTypes(ctx, active)
	}

	q := r.URL.Query()
	view := &piPlannerView{Targets: piplan.AllTargets()}
	data.PIPlanner = view

	// Target: ?product= name (also accept ?target=).
	target := strings.TrimSpace(q.Get("product"))
	if target == "" {
		target = strings.TrimSpace(q.Get("target"))
	}
	if target == "" {
		target = "Broadcast Node"
	}
	// Validate against the catalogue; unknown falls back.
	if _, ok := piplan.AllCommodities[target]; !ok || piplan.TierOf(target) == piplan.TierRaw {
		target = "Broadcast Node"
	}
	view.Target = target

	view.Amount = 10
	if n, err := strconv.ParseInt(q.Get("amount"), 10, 64); err == nil && n >= 1 && n <= 100000 {
		view.Amount = n
	}
	view.AmountStr = strconv.FormatInt(view.Amount, 10)
	view.CCLevel = 5
	if n, err := strconv.Atoi(q.Get("cc")); err == nil && n >= 0 && n <= 5 {
		view.CCLevel = n
	}
	view.Heads = 10
	if n, err := strconv.Atoi(q.Get("heads")); err == nil && n >= 1 && n <= 10 {
		view.Heads = n
	}
	view.Yield = 12000
	if n, err := strconv.ParseInt(q.Get("yield"), 10, 64); err == nil && n >= 100 && n <= 1000000 {
		view.Yield = n
	}
	view.TaxPct = 10
	if f, err := strconv.ParseFloat(q.Get("tax"), 64); err == nil && f >= 0 && f <= 100 {
		view.TaxPct = f
	}
	view.TaxStr = strconv.FormatFloat(view.TaxPct, 'f', -1, 64)

	// Selected planets: explicit ?planets= wins (form checkboxes
	// repeat the key; links may comma-join it; a present-but-empty
	// value means "none ticked"); else owned types; else the
	// minimal cover for the target.
	selected := map[string]bool{}
	if _, explicit := q["planets"]; explicit {
		for _, raw := range q["planets"] {
			for _, p := range strings.Split(raw, ",") {
				if p = strings.TrimSpace(p); p != "" {
					selected[titleCasePlanet(p)] = true
				}
			}
		}
	} else if len(ownedPlanets) > 0 {
		for _, p := range ownedPlanets {
			selected[p] = true
		}
	} else {
		for _, p := range piplan.MinimalPlanets(target) {
			selected[p] = true
		}
	}
	ownedSet := map[string]bool{}
	for _, p := range ownedPlanets {
		ownedSet[p] = true
	}
	for _, pt := range piplan.PlanetTypes {
		view.PlanetChecks = append(view.PlanetChecks, piPlanetCheck{
			Name: pt, Checked: selected[pt], Owned: ownedSet[pt],
		})
	}
	view.OwnedPlanets = ownedPlanets
	view.OwnedNote = ownedNote

	var selList []string
	for _, c := range view.PlanetChecks {
		if c.Checked {
			selList = append(selList, c.Name)
		}
	}

	prices, priceNote := app.piPricesByName(ctx)
	view.PricesNote = priceNote

	settings := piplan.Settings{
		CCLevel: view.CCLevel, HeadsPerECU: view.Heads,
		ECUYield: float64(view.Yield), TaxRate: view.TaxPct / 100, Hours: 24,
	}
	plan, perr := piplan.PlanFor(target, view.Amount, settings, prices)
	if perr != nil {
		data.Error = perr.Error()
		app.render(ctx, w, http.StatusOK, "piplanner.html", data)
		return
	}
	view.HasPlan = true
	view.PlanTarget = target
	fillPIPlanDisplay(view, plan, prices)
	view.Graph, view.GraphJSON = buildPIGraph(target, selList, prices)
	// Coverage summary.
	cov := piplan.CoverageFor(target, selList)
	if cov.Complete {
		view.CoverageText = fmt.Sprintf("Covered — your %d selected planet type(s) supply all %d raw resources.", len(selList), len(cov.Required))
		view.CoverageClass = "pos"
	} else if len(selList) == 0 {
		view.CoverageText = "No planet types selected — tick planets above to check coverage."
		view.CoverageClass = ""
	} else {
		view.CoverageText = fmt.Sprintf("Not covered — %d of %d raw resources available.", len(cov.Covered), len(cov.Required))
		view.CoverageClass = "neg"
	}
	if len(cov.Missing) > 0 {
		view.MissingText = "Missing: " + strings.Join(cov.Missing, ", ")
	}
	view.MinimalText = "Smallest covering set: " + strings.Join(piplan.MinimalPlanets(target), ", ")
	if one, ok := piplan.SinglePlanet(target); ok {
		view.SingleText = "Single-planet possible on " + one + "."
	}
	view.Profitable = piProfitableChains(prices, settings)
	view.FacilityNote = "High-Tech (P4) only on Barren/Temperate · Advanced anywhere · Basic on the extractor planet. Taxes use CCP taxable values (R0 5 · P1 400 · P2 7,200 · P3 60,000 · P4 1,200,000 ISK); import at half the export rate."

	app.render(ctx, w, http.StatusOK, "piplanner.html", data)
}

func titleCasePlanet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func (app *Application) piOwnedPlanetTypes(ctx context.Context, ch db.Character) ([]string, string) {
	var colonies esi.Colonies
	if !app.loadCorpSnapshot(ctx, ch.CharacterID, esi.SnapPlanets, &colonies) {
		if app.piNotEnabled(ctx, ch) {
			return nil, "Planetary scope not enabled for this character yet — re-link to pre-fill from your colonies. You can still plan standalone."
		}
		return nil, "Colonies still warming up — plan standalone, or check back once Sync has landed them."
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range colonies {
		t := displayPlanetType(c.PlanetType)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, "No colonies yet for this character — plan standalone below."
	}
	return out, fmt.Sprintf("Pre-filled from %s's colonies (%s). Tick/untick to what-if other planets.", ch.Name, strings.Join(out, ", "))
}

// piPricesByName resolves guide prices for every PI commodity.
// Names resolve to type IDs through the SDE; the guide map then
// prices them. Missing SDE rows simply stay unpriced.
func (app *Application) piPricesByName(ctx context.Context) (map[string]float64, string) {
	names := make([]string, 0, len(piplan.AllCommodities))
	for n := range piplan.AllCommodities {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]float64{}
	guide := app.valuationPrices(ctx)
	if guide == nil {
		return out, "No market guide yet — prices land with the first market refresh; quantities, factories and taxes already work."
	}
	rows, err := app.queries.ListSDETypesByNames(ctx, names)
	if err != nil {
		logging.Errorf("piplanner: resolve type IDs: %v", err)
		return out, "Prices unavailable — type lookup failed; quantities, factories and taxes already work."
	}
	byID := map[int64]string{}
	for _, row := range rows {
		byID[row.TypeID] = row.Name
	}
	priced := 0
	for id, name := range byID {
		p, hit := guide[id]
		if !hit {
			continue
		}
		v := p.AveragePrice
		if v <= 0 {
			v = p.AdjustedPrice
		}
		if v > 0 {
			out[name] = v
			priced++
		}
	}
	if priced == 0 {
		return out, "Market guide has no PI prices yet — quantities, factories and taxes already work."
	}
	return out, fmt.Sprintf("Prices: cluster guide average for %d of %d PI commodities. Missing prices show as —, never zero-filled.", priced, len(names))
}

func fillPIPlanDisplay(view *piPlannerView, plan *piplan.Plan, prices map[string]float64) {
	isk := func(f float64, known bool) string {
		if !known {
			return "—"
		}
		return esi.FormatISK(f)
	}
	// Lines already ordered target-first by the engine call site;
	// re-sort display: tier descending, target first within tier.
	lines := append([]piplan.Line(nil), plan.Lines...)
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Name == plan.Target {
			return true
		}
		if lines[j].Name == plan.Target {
			return false
		}
		ri, rj := piTierRank(lines[i].Tier), piTierRank(lines[j].Tier)
		if ri != rj {
			return ri > rj
		}
		return lines[i].Name < lines[j].Name
	})
	for _, ln := range lines {
		row := piLineRow{
			Name: ln.Name, Tier: string(ln.Tier),
			Needed: formatQty(ln.Needed), PerHour: formatQty(ln.PerHour),
			Volume: fmt.Sprintf("%.1f m³", ln.Volume),
		}
		if ln.Tier == piplan.TierRaw {
			row.Factories = "—"
			row.Facility = "Extractor"
		} else {
			row.Factories = strconv.Itoa(ln.Factories)
			row.Facility = string(ln.Facility)
		}
		if ln.PriceKnown {
			row.Price = esi.FormatISK(ln.Price)
			row.Value = esi.FormatISK(ln.LineValue)
		} else {
			row.Price, row.Value = "—", "—"
		}
		view.Lines = append(view.Lines, row)
	}
	for _, c := range plan.Colonies {
		view.Colonies = append(view.Colonies, piColonyRow{
			Title: c.Title, Planet: c.Planet, Detail: c.Detail,
			CPU:  fmt.Sprintf("CPU %s / %s tf · Power %s / %s MW", esi.FormatInt(c.CPUUsed), esi.FormatInt(c.CPUCap), esi.FormatInt(c.PGUsed), esi.FormatInt(c.PGCap)),
			Fits: c.Fits, Breakdown: c.Breakdown,
		})
	}
	view.Revenue = isk(plan.Revenue, plan.RevenueKnown)
	view.RawCost = isk(plan.RawCost, plan.RawCostKnown)
	view.StepCost = isk(plan.StepCost, plan.StepCostKnown)
	view.ProfitWhole = isk(plan.ProfitWhole, plan.RevenueKnown && plan.RawCostKnown)
	view.MarginWhole = "—"
	if plan.RevenueKnown && plan.RawCostKnown {
		view.MarginWhole = fmt.Sprintf("%+.1f%%", plan.MarginWhole)
		if plan.ProfitWhole > 0 {
			view.ProfitWholeCls = "pos"
		} else if plan.ProfitWhole < 0 {
			view.ProfitWholeCls = "neg"
		}
	}
	view.ProfitStep = isk(plan.ProfitStep, plan.RevenueKnown && plan.StepCostKnown)
	view.MarginStep = "—"
	if plan.RevenueKnown && plan.StepCostKnown {
		view.MarginStep = fmt.Sprintf("%+.1f%%", plan.MarginStep)
	}
	view.ExportTax = esi.FormatISK(plan.ExportTax)
	view.ImportTax = esi.FormatISK(plan.ImportTax)
}

func piTierRank(t piplan.Tier) int {
	switch t {
	case piplan.TierP4:
		return 4
	case piplan.TierP3:
		return 3
	case piplan.TierP2:
		return 2
	case piplan.TierP1:
		return 1
	}
	return 0
}

func formatQty(f float64) string {
	if f == math.Trunc(f) {
		return esi.FormatInt(int64(f))
	}
	return fmt.Sprintf("%.1f", f)
}

// buildPIGraph shapes the five-tier dependency map. Chain nodes
// (inputs of target) highlight; downstream consumers dim less;
// everything else dims. Coverage tints raws/P1s.
func buildPIGraph(target string, selected []string, prices map[string]float64) ([]piGraphCol, string) {
	chain := map[string]bool{}
	for _, n := range piplan.ChainNodes(target) {
		chain[n] = true
	}
	// Downstream: commodities that (transitively) consume target.
	downstream := map[string]bool{}
	for name := range piplan.AllCommodities {
		if name == target {
			continue
		}
		for _, n := range piplan.ChainNodes(name) {
			if n == target {
				downstream[name] = true
				break
			}
		}
	}
	covAvail := map[string]bool{}
	for _, p := range selected {
		for _, raw := range piplan.PlanetYields[p] {
			covAvail[raw] = true
		}
	}
	// P1 covered when its raw is available.
	p1Covered := map[string]bool{}
	for _, p1 := range piplan.ByTier(piplan.TierP1) {
		if raw, ok := piplan.RawForP1(p1); ok && covAvail[raw] {
			p1Covered[p1] = true
		}
	}
	mk := func(name string) piGraphNode {
		n := piGraphNode{Name: name, Tier: string(piplan.TierOf(name))}
		if p, ok := prices[name]; ok && p > 0 {
			n.Price = esi.FormatISK(p)
		}
		switch {
		case name == target:
			n.Class = "selected"
		case chain[name]:
			n.Class = "input"
			// Tint chain P1/raws by coverage when planets chosen.
			if len(selected) > 0 {
				if piplan.TierOf(name) == piplan.TierRaw && !covAvail[name] {
					n.Class = "input missing"
				}
				if piplan.TierOf(name) == piplan.TierP1 && !p1Covered[name] {
					n.Class = "input missing"
				}
			}
		case downstream[name]:
			n.Class = "output"
		default:
			n.Class = "dimmed"
		}
		if piplan.TierOf(name) == piplan.TierRaw {
			n.Planets = strings.Join(piplan.RawSources[name], ", ")
		}
		return n
	}
	cols := []piGraphCol{
		{Title: "Planets", Nodes: nil},
		{Title: "Raw (R0)", Nodes: nil},
		{Title: "P1", Nodes: nil},
		{Title: "P2", Nodes: nil},
		{Title: "P3", Nodes: nil},
		{Title: "P4", Nodes: nil},
	}
	sel := map[string]bool{}
	for _, p := range selected {
		sel[p] = true
	}
	for _, pt := range piplan.PlanetTypes {
		cls := "dimmed"
		if sel[pt] {
			cls = "covered"
		}
		cols[0].Nodes = append(cols[0].Nodes, piGraphNode{Name: pt, Tier: "Planet", Class: cls})
	}
	for i, tier := range []piplan.Tier{piplan.TierRaw, piplan.TierP1, piplan.TierP2, piplan.TierP3, piplan.TierP4} {
		for _, name := range piplan.ByTier(tier) {
			cols[i+1].Nodes = append(cols[i+1].Nodes, mk(name))
		}
	}
	// JSON for pi.js highlighting (names + inputs only).
	type gnode struct {
		Name   string   `json:"name"`
		Inputs []string `json:"inputs"`
	}
	graph := map[string]gnode{}
	for name := range piplan.AllCommodities {
		var ins []string
		for in := range piplan.InputsOf(name) {
			ins = append(ins, in)
		}
		sort.Strings(ins)
		graph[name] = gnode{Name: name, Inputs: ins}
	}
	blob, _ := json.Marshal(graph)
	return cols, string(blob)
}

func piProfitableChains(prices map[string]float64, s piplan.Settings) []piProfitRow {
	var rows []piProfitRow
	for _, tier := range []piplan.Tier{piplan.TierP4, piplan.TierP3} {
		for _, name := range piplan.ByTier(tier) {
			plan, err := piplan.PlanFor(name, 10, s, prices)
			if err != nil || !plan.RevenueKnown || !plan.RawCostKnown || plan.Revenue <= 0 {
				continue
			}
			cls := ""
			if plan.ProfitWhole > 0 {
				cls = "pos"
			} else if plan.ProfitWhole < 0 {
				cls = "neg"
			}
			rows = append(rows, piProfitRow{
				Name: name, Margin: fmt.Sprintf("%+.1f%%", plan.MarginWhole), MarginVal: plan.MarginWhole,
				Profit: esi.FormatISK(plan.ProfitWhole), Class: cls,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].MarginVal > rows[j].MarginVal })
	if len(rows) > 8 {
		rows = rows[:8]
	}
	// Attach URLs preserving nothing but the pick.
	for i := range rows {
		rows[i].URL = "/industry/pi/?product=" + strings.ReplaceAll(rows[i].Name, " ", "+")
	}
	return rows
}
