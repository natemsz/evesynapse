package piplanner

import (
	"context"
	"fmt"
	json "github.com/goccy/go-json"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
	"evesynapse/internal/piplan"
)

// ---------------------------------------------------------------------------
// PI Planner (/industry/pi/): full planetary-interaction simulator
// under the Industry hub. Pick a P1–P4
// product, set how much to make, and get the whole chain — raw
// needs, factory counts, extractor colonies, CPU/Power fit,
// POCO taxes and market economics. Prices follow the house order:
// The Forge's swept typical sell, then the stored estimated sell,
// then unpriced (piPricesByName); colonies pre-fill planet types.
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
	// On names where a chain node is made: for a raw or a P1, the
	// selected planet types that supply it (every source when none
	// does); for a factory product, the planets its facility may
	// stand on. Empty off the chain.
	On string
}

type piGraphCol struct {
	Title string
	Nodes []piGraphNode
}

type piLineRow struct {
	Name      string
	TypeID    int64 // 0 when the SDE has no row for the name
	Tier      string
	TierKey   string // r0, p1 ... p4: the tier badge's colour
	Target    bool
	FactPct   int // factories against the plan's largest count, for the meter
	Needed    string
	PerHour   string
	Factories string
	Facility  string
	Volume    string
	Price     string
	Value     string
	Spark     template.HTML // the unit price's hover card (The Forge's stored 30-day line)
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
	On        bool // the product being planned
	BarPct    int  // margin against the best margin listed, for the bar
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
	PriceLine      string // the strip's provenance: where prices come from and how fresh
	TargetTier     string
	TargetTierKey  string
	PlanetsText    string // the planet types ticked, for the summary strip
	Profitable     []piProfitRow
	FacilityNote   string
}

// PageData is the view model passed to piplanner.html.
type PageData struct {
	module.ShellData
	PIPlanner      *piPlannerView
	PIPlannerChars []module.CharLink
}

// Shell returns the embedded ShellData pointer for render.
func (p *PageData) Shell() *module.ShellData {
	return &p.ShellData
}

// HandlePIPlanner serves the PI planner page over the given foundation Deps.
func HandlePIPlanner(deps module.Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		data := PageData{ShellData: deps.PublicShell(ctx)} // public: no session needed
		data.HeadMeta = deps.HeadMeta(module.PageMeta{Title: "PI Planner", Description: "Plan EVE Online planetary industry: chain extractors and factories from raw planet goods up to finished products, step by step.", CanonicalPath: "/industry/pi/"})

		_, active, links, err := deps.PickCharacter(ctx, r, "/industry/pi/")
		if err != nil {
			logging.Errorf("piplanner: list characters: %v", err)
			data.Error = "Could not load PI planner data; check the server log."
			deps.Render(ctx, w, http.StatusOK, "piplanner.html", &data)
			return
		}
		// Standalone: no character is fine. When one
		// exists, its colonies pre-fill the planet picker.
		var ownedPlanets []string
		ownedNote := ""
		if links != nil && active.CharacterID != 0 {
			data.PIPlannerChars = links
			ownedPlanets, ownedNote = piOwnedPlanetTypes(ctx, deps, active)
		}

		q := r.URL.Query()
		view := &piPlannerView{Targets: piplan.AllTargets()}
		data.PIPlanner = view

		// Target: ?product= is the page's one parameter (its form field,
		// pi.js and every link in). ?target= is still read for links
		// made before that was settled.
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

		// Selected planets: ?cover=1 (a product just picked) ticks the
		// minimal cover for the target, so the page shows which planet
		// types that product needs; else explicit ?planets= wins (form
		// checkboxes repeat the key; links may comma-join it; a
		// present-but-empty value means "none ticked"); else owned
		// types; else the minimal cover.
		selected := map[string]bool{}
		if q.Get("cover") == "1" {
			for _, p := range piplan.MinimalPlanets(target) {
				selected[p] = true
			}
		} else if _, explicit := q["planets"]; explicit {
			for _, raw := range q["planets"] {
				for _, p := range strings.Split(raw, ",") {
					if p = strings.TrimSpace(p); p != "" {
						selected[piplan.TitlePlanet(p)] = true
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

		prices, ids, priceLine, priceNote := piPricesByName(ctx, deps, time.Now())
		view.PriceLine, view.PricesNote = priceLine, priceNote
		view.TargetTier = string(piplan.TierOf(target))
		view.TargetTierKey = strings.ToLower(view.TargetTier)
		view.PlanetsText = strings.Join(selList, ", ")

		settings := piplan.Settings{
			CCLevel: view.CCLevel, HeadsPerECU: view.Heads,
			ECUYield: float64(view.Yield), TaxRate: view.TaxPct / 100, Hours: 24,
			Volumes: VolumesByName(ctx, deps),
		}
		plan, perr := piplan.PlanFor(target, view.Amount, settings, prices)
		if perr != nil {
			data.Error = perr.Error()
			deps.Render(ctx, w, http.StatusOK, "piplanner.html", &data)
			return
		}
		view.HasPlan = true
		view.PlanTarget = target
		fillPIPlanDisplay(view, plan, ids)
		view.Graph, view.GraphJSON = buildPIGraph(target, selList, prices)
		// Coverage summary.
		cov := piplan.CoverageFor(target, selList)
		if cov.Complete {
			view.CoverageText = fmt.Sprintf("Covered: your %d selected planet type(s) supply all %d raw resources.", len(selList), len(cov.Required))
			view.CoverageClass = "pos"
		} else if len(selList) == 0 {
			view.CoverageText = "No planet types selected. Tick planets above to check coverage."
			view.CoverageClass = ""
		} else {
			view.CoverageText = fmt.Sprintf("Not covered: %d of %d raw resources available.", len(cov.Covered), len(cov.Required))
			view.CoverageClass = "neg"
		}
		if len(cov.Missing) > 0 {
			view.MissingText = "Missing: " + strings.Join(cov.Missing, ", ")
		}
		view.MinimalText = "Smallest covering set: " + strings.Join(piplan.MinimalPlanets(target), ", ")
		if one, ok := piplan.SinglePlanet(target); ok {
			view.SingleText = "Single-planet possible on " + one + "."
		}
		view.Profitable = piProfitableChains(prices, settings, target)
		keys := make([]module.SparkKey, len(view.Lines))
		for i, l := range view.Lines {
			keys[i] = module.SparkKey{RegionID: piForgeRegionID, TypeID: l.TypeID}
		}
		if cards := deps.PriceSparks(ctx, keys); len(cards) > 0 {
			for i := range view.Lines {
				view.Lines[i].Spark = cards[keys[i]]
			}
		}
		view.FacilityNote = "High-Tech (P4) only on Barren or Temperate, Advanced anywhere, Basic on the extractor planet. Taxes use CCP's taxable values (R0 5, P1 400, P2 7,200, P3 60,000, P4 1,200,000 ISK); import is taxed at half the export rate."

		deps.Render(ctx, w, http.StatusOK, "piplanner.html", &data)
	}
}

func piOwnedPlanetTypes(ctx context.Context, deps module.Deps, ch db.Character) ([]string, string) {
	var colonies esi.Colonies
	if !deps.LoadCorpSnapshot(ctx, ch.CharacterID, esi.SnapPlanets, &colonies) {
		if deps.PINotEnabled(ctx, ch) {
			return nil, "Planetary scope not enabled for this character yet. Re-link to pre-fill from your colonies. You can still plan standalone."
		}
		return nil, "Your colonies are still syncing. Plan without them for now, or check back after the next sync."
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
		return nil, "No colonies yet for this character. Plan standalone below."
	}
	return out, fmt.Sprintf("Pre-filled from %s's colonies (%s). Tick/untick to what-if other planets.", ch.Name, strings.Join(out, ", "))
}

// VolumesByName reads every PI commodity's volume from the
// imported SDE, by name. The SDE is the truth for volumes; the
// engine's built-in figures only stand in for a commodity the
// SDE does not carry (or before the SDE is imported), so a
// failed or empty read just returns less.
func VolumesByName(ctx context.Context, deps module.Deps) map[string]float64 {
	names := make([]string, 0, len(piplan.AllCommodities))
	for n := range piplan.AllCommodities {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]float64{}
	types, err := deps.DB().ListSDETypesByNames(ctx, names)
	if err != nil {
		logging.Errorf("piplanner: resolve type IDs for volumes: %v", err)
		return out
	}
	byID := make(map[int64]string, len(types))
	ids := make([]int64, 0, len(types))
	for _, row := range types {
		byID[row.TypeID] = row.Name
		ids = append(ids, row.TypeID)
	}
	physics, err := deps.DB().ListSDETypePhysicsByIDs(ctx, ids)
	if err != nil {
		logging.Errorf("piplanner: read volumes: %v", err)
		return out
	}
	for _, row := range physics {
		if row.Volume > 0 {
			out[byID[row.TypeID]] = row.Volume
		}
	}
	return out
}

// piForgeRegionID is The Forge, whose swept book is the first
// tier of the house price order.
const piForgeRegionID = 10000002

// piPricesByName prices every PI commodity in the house order: The
// Forge's swept typical sell, then the stored estimated sell (only
// where an evidence tier stands behind it), then unpriced. The
// guide's average prices nothing. Names resolve to type IDs through
// the SDE; both reads are bounded to the PI catalogue (about 80
// types). It returns the prices, the type IDs by name (the
// material links), the strip's provenance line and a note on gaps.
func piPricesByName(ctx context.Context, deps module.Deps, now time.Time) (map[string]float64, map[string]int64, string, string) {
	names := make([]string, 0, len(piplan.AllCommodities))
	for n := range piplan.AllCommodities {
		names = append(names, n)
	}
	sort.Strings(names)
	out := map[string]float64{}
	idByName := map[string]int64{}
	rows, err := deps.DB().ListSDETypesByNames(ctx, names)
	if err != nil {
		logging.Errorf("piplanner: resolve type IDs: %v", err)
		return out, idByName, "Prices unavailable", "The item lookup failed, so nothing is priced. Quantities, factories and taxes still work."
	}
	nameByID := map[int64]string{}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		nameByID[row.TypeID] = row.Name
		idByName[row.Name] = row.TypeID
		ids = append(ids, row.TypeID)
	}
	var newest time.Time
	fromForge := 0
	if stats, err := deps.DB().ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{RegionID: piForgeRegionID, Column2: ids}); err != nil {
		logging.Errorf("piplanner: forge stats: %v", err)
	} else {
		for _, row := range stats {
			if row.TypicalSell > 0 {
				out[nameByID[row.TypeID]] = row.TypicalSell
				fromForge++
				if row.UpdatedAt.After(newest) {
					newest = row.UpdatedAt
				}
			}
		}
	}
	estimated := 0
	if tvs, err := deps.DB().ListTrueValuesForTypes(ctx, ids); err != nil {
		logging.Errorf("piplanner: stored estimates: %v", err)
	} else {
		for _, row := range tvs {
			name := nameByID[row.TypeID]
			if _, priced := out[name]; priced {
				continue
			}
			if row.EstSell.Valid && row.EstSell.Float64 > 0 && row.SellSource != "" {
				out[name] = row.EstSell.Float64
				estimated++
			}
		}
	}
	priced := fromForge + estimated
	if priced == 0 {
		return out, idByName, "No prices yet", "No PI commodity has a Forge sell price or a stored estimate yet. Quantities, factories and taxes already work."
	}
	line := "Prices from The Forge"
	if !newest.IsZero() {
		line += ", updated " + piAgo(now.Sub(newest))
	}
	note := ""
	if estimated > 0 {
		note = fmt.Sprintf("%d of %d PI commodities price at The Forge's typical sell; %d with no Forge price use the stored estimated sell.", fromForge, len(names), estimated)
	}
	if missing := len(names) - priced; missing > 0 {
		if note != "" {
			note += " "
		}
		note += fmt.Sprintf("%d have no price yet and show a dash, never a zero.", missing)
	}
	return out, idByName, line, note
}

// piAgo says how long ago a price row landed, in the strip's words.
func piAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	}
	return plural(int(d/(24*time.Hour)), "day") + " ago"
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

func fillPIPlanDisplay(view *piPlannerView, plan *piplan.Plan, ids map[string]int64) {
	// The summary figures read in whole ISK: they sit on stat tiles
	// and in prose, where cents only crowd the number. The table
	// keeps them.
	isk := func(f float64, known bool) string {
		if !known {
			return "—"
		}
		return esi.FormatInt(int64(math.Round(f)))
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
		ri, rj := piplan.TierRank(lines[i].Tier), piplan.TierRank(lines[j].Tier)
		if ri != rj {
			return ri > rj
		}
		return lines[i].Name < lines[j].Name
	})
	maxFact := 1
	for _, ln := range lines {
		if ln.Tier != piplan.TierRaw && ln.Factories > maxFact {
			maxFact = ln.Factories
		}
	}
	for _, ln := range lines {
		row := piLineRow{
			Name: ln.Name, TypeID: ids[ln.Name], Tier: string(ln.Tier),
			TierKey: strings.ToLower(string(ln.Tier)), Target: ln.Name == plan.Target,
			Needed: formatQty(ln.Needed), PerHour: formatQty(ln.PerHour),
			Volume: fmt.Sprintf("%.1f m³", ln.Volume),
		}
		if ln.Tier == piplan.TierRaw {
			row.Factories = "—"
			row.Facility = "Extractor"
		} else {
			row.Factories = strconv.Itoa(ln.Factories)
			row.FactPct = ln.Factories * 100 / maxFact
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
	view.ExportTax = isk(plan.ExportTax, true)
	view.ImportTax = isk(plan.ImportTax, true)
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
		if name == target || chain[name] {
			n.On = piProducedOn(name, selected)
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
	// A planet type that yields any raw on the chain is a source
	// for it, ticked or not, so the column shows every planet the
	// target could be made from beside the ones selected.
	source := map[string]bool{}
	for _, raw := range piplan.RequiredRaws(target) {
		for _, p := range piplan.RawSources[raw] {
			source[p] = true
		}
	}
	for _, pt := range piplan.PlanetTypes {
		cls := "dimmed"
		if sel[pt] {
			cls = "covered"
		} else if source[pt] {
			cls = "source"
		}
		if source[pt] {
			cls += " can-supply"
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

func piProfitableChains(prices map[string]float64, s piplan.Settings, target string) []piProfitRow {
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
	// Attach URLs preserving nothing but the pick; bars run against
	// the best margin listed, and a loss draws no bar.
	best := 0.0
	if len(rows) > 0 {
		best = rows[0].MarginVal
	}
	for i := range rows {
		rows[i].URL = "/industry/pi/?product=" + strings.ReplaceAll(rows[i].Name, " ", "+")
		rows[i].On = rows[i].Name == target
		if best > 0 && rows[i].MarginVal > 0 {
			rows[i].BarPct = int(math.Round(rows[i].MarginVal / best * 100))
		}
	}
	return rows
}

// piProducedOn names the planet types a chain node is made on (see
// piGraphNode.On). pi.js recomputes the same line live as planets
// are ticked: a change to either side must change both.
func piProducedOn(name string, selected []string) string {
	raw := ""
	switch piplan.TierOf(name) {
	case piplan.TierRaw:
		raw = name
	case piplan.TierP1:
		raw, _ = piplan.RawForP1(name)
	default:
		if sch, ok := piplan.Schematics[name]; ok {
			if only := piplan.FactoryPlanetOnly(sch.Facility); only != "Any" {
				return "factory on " + only
			}
		}
		return "factory on any planet"
	}
	sel := map[string]bool{}
	for _, p := range selected {
		sel[p] = true
	}
	var on []string
	for _, p := range piplan.RawSources[raw] {
		if sel[p] {
			on = append(on, p)
		}
	}
	if len(on) == 0 {
		on = piplan.RawSources[raw]
	}
	return "on " + strings.Join(on, ", ")
}

func displayPlanetType(raw string) string {
	if raw == "" {
		return ""
	}
	return strings.ToUpper(raw[:1]) + raw[1:]
}

// Module exposes the PI planner module registration contract.
var Module = module.Module{
	ID:       "piplanner",
	Title:    "PI Planner",
	Requires: []string{},
	// The planner works for anyone, signed in or not.
	Scopes: []module.Scope{},
	EnhanceScopes: []module.Scope{
		{Scope: "esi-planets.manage_planets.v1", Unlocks: "Your colonies fill in the planet picker and the coverage check."},
	},
	Sitemap:    []module.SitemapEntry{{Path: "/industry/pi/", ChangeFreq: "monthly", Priority: "0.8"}},
	Activities: []string{"industry"},
	Nav: []module.NavSection{
		{
			Section: "industry",
			Entries: []module.NavEntry{
				{Href: "/industry/pi/", Label: "PI Planner", Pages: []string{"piplanner.html"}},
			},
		},
	},
	Routes: func(r chi.Router, deps module.Deps) {
		r.With(deps.AnonPages()).Get("/industry/pi/", HandlePIPlanner(deps))
	},
	Pages: []module.Page{
		{
			Template: "piplanner.html",
			Section:  "industry",
			Scripts:  []string{"/static/pi.js"},
		},
	},
}
