package buildplan

// Engine tests: bill-of-materials expansion against fixture recipes.
// The EVE material formula (ME curve, base-1 floor), whole-run child
// production, the cycle guard, owned-blueprint ME/TE autofill versus
// explicit overrides, single-allocation stockpile netting, and price
// honesty (a missing price is an unknown, never an invented zero).

import (
	"testing"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Engine fixtures.
//
//   1001 Widget     ← bp 2001: 2× Component(1002) + 10× Mineral(34)
//   1002 Component  ← bp 2002: 5× Mineral(34)
//   1005 Bulk Part  ← bp 2004: 100 units per run from 1× Mineral
//   1006 Bulk Thing ← bp 2005: 250× Bulk Part(1005)
//   1007 Loop A     ← bp 2006: 1× Loop B(1008)
//   1008 Loop B     ← bp 2007: 1× Loop A(1007)
//   1009 Solo       ← bp 2008: 1× Mineral(34)   (base-1 floor)
// ---------------------------------------------------------------------------

type fixturePlannerSource map[int64]*Blueprint

func (f fixturePlannerSource) BlueprintForProduct(productTypeID int64) (*Blueprint, bool) {
	bp, ok := f[productTypeID]
	return bp, ok
}

func fixtureRecipes() fixturePlannerSource {
	return fixturePlannerSource{
		1001: {BlueprintTypeID: 2001, ProductTypeID: 1001, ProductQuantity: 1, TimeSeconds: 600,
			Materials: []Material{{TypeID: 1002, Quantity: 2}, {TypeID: 34, Quantity: 10}}},
		1002: {BlueprintTypeID: 2002, ProductTypeID: 1002, ProductQuantity: 1, TimeSeconds: 300,
			Materials: []Material{{TypeID: 34, Quantity: 5}}},
		1005: {BlueprintTypeID: 2004, ProductTypeID: 1005, ProductQuantity: 100, TimeSeconds: 60,
			Materials: []Material{{TypeID: 34, Quantity: 1}}},
		1006: {BlueprintTypeID: 2005, ProductTypeID: 1006, ProductQuantity: 1, TimeSeconds: 900,
			Materials: []Material{{TypeID: 1005, Quantity: 250}}},
		1007: {BlueprintTypeID: 2006, ProductTypeID: 1007, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []Material{{TypeID: 1008, Quantity: 1}}},
		1008: {BlueprintTypeID: 2007, ProductTypeID: 1008, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []Material{{TypeID: 1007, Quantity: 1}}},
		1009: {BlueprintTypeID: 2008, ProductTypeID: 1009, ProductQuantity: 1, TimeSeconds: 10,
			Materials: []Material{{TypeID: 34, Quantity: 1}}},
	}
}

func findPlanNode(root *Node, typeID int64) *Node {
	var hit *Node
	Walk(root, func(n *Node) {
		if hit == nil && n.TypeID == typeID {
			hit = n
		}
	})
	return hit
}

func leafQty(root *Node, typeID int64) (required, toBuy int64) {
	Walk(root, func(n *Node) {
		if n.TypeID == typeID && n.Blueprint == nil {
			required += n.RequiredQty
			toBuy += n.Shortfall
		}
	})
	return required, toBuy
}

func TestMaterialForRunsFormula(t *testing.T) {
	cases := []struct {
		base, runs int64
		me         int
		want       int64
	}{
		{10, 1, 0, 10},    // no ME
		{10, 1, 10, 9},    // full ME discount
		{2, 1, 10, 2},     // ceil(1.8)
		{5, 2, 10, 9},     // ceil(5×2×0.9)
		{1, 100, 10, 100}, // base-1 floor: never below runs
		{1, 1, 10, 1},
		{3, 7, 5, 20}, // ceil(3×7×0.95) = ceil(19.95)
		{86, 3, 0, 258},
		{86, 3, 10, 233}, // ceil(232.2)
	}
	for _, c := range cases {
		if got := MaterialForRuns(c.base, c.runs, c.me); got != c.want {
			t.Errorf("materialForRuns(%d, %d, %d) = %d, want %d", c.base, c.runs, c.me, got, c.want)
		}
	}
	if got := JobSeconds(600, 2, 20); got != 960 { // 600×2×0.8
		t.Errorf("jobSeconds(600,2,20) = %d, want 960", got)
	}
	if got := JobSeconds(300, 3, 0); got != 900 {
		t.Errorf("jobSeconds(300,3,0) = %d, want 900", got)
	}
}

func TestPlanExpansionMath(t *testing.T) {
	res, err := Build(fixtureRecipes(), 1001, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	root := res.Root
	if root.Runs != 1 || root.ProducedQty != 1 {
		t.Errorf("root: runs %d produced %d, want 1/1", root.Runs, root.ProducedQty)
	}
	comp := findPlanNode(root, 1002)
	if comp == nil || comp.Runs != 2 {
		t.Fatalf("component node: %+v, want 2 runs", comp)
	}
	// Mineral: 10 direct + 2 runs × 5 under the component = 20.
	if req, buy := leafQty(root, 34); req != 20 || buy != 20 {
		t.Errorf("mineral: required %d to-buy %d, want 20/20", req, buy)
	}
	// Total time: root 600 + component 2×300 = 1200s sequential.
	if res.TotalSeconds != 1200 {
		t.Errorf("total seconds: got %d, want 1200", res.TotalSeconds)
	}
	if len(res.Shopping) != 1 || res.Shopping[0].TypeID != 34 || res.Shopping[0].Quantity != 20 {
		t.Errorf("shopping: %+v, want one 20× Mineral line", res.Shopping)
	}
}

func TestMaterialForRunsFactored(t *testing.T) {
	// Factor 1 (or unset) is exactly the plain formula.
	for _, f := range []float64{0, 1} {
		if got := MaterialForRunsFactored(10, 3, 5, f); got != MaterialForRuns(10, 3, 5) {
			t.Errorf("factor %v: got %d, want %d", f, got, MaterialForRuns(10, 3, 5))
		}
	}
	// Raitaru + T1 rigs in hisec: ×0.99 × (1 − 0.02) ≈ ×0.9702.
	// 5 × 2 runs at ME 8: 9.2 plain → ceil(9.2 × 0.9702) = 9.
	if got := MaterialForRunsFactored(5, 2, 8, 0.99*0.98); got != 9 {
		t.Errorf("factored: got %d, want 9", got)
	}
	// The base-1 floor survives factoring: 1-per-run never drops
	// below one per run.
	if got := MaterialForRunsFactored(1, 4, 10, 0.5); got != 4 {
		t.Errorf("base-1 floor: got %d, want 4", got)
	}
}

func TestPlanFacilityFactors(t *testing.T) {
	// Raitaru role bonus with T1 rigs at hisec multipliers:
	// materials ×0.99×0.98, time ×0.85×0.80, cost ×0.97.
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs:               1,
		FacilityMatFactor:  0.99 * 0.98,
		FacilityTimeFactor: 0.85 * 0.80,
		FacilityCostFactor: 0.97,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	// Mineral: widget 10 × 0.9702 → 10 (ceil 9.702); component
	// 5 × 2 × 0.9702 → 10 (ceil 9.702). Total 20 — the ceiling
	// eats the bonus at this scale, so drop runs up: at 10 runs
	// the widget needs ceil(100 × 0.9702) = 98, not 100.
	if req, _ := leafQty(res.Root, 34); req != 20 {
		t.Errorf("mineral at 1 run: got %d, want 20", req)
	}
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:              10,
		FacilityMatFactor: 0.99 * 0.98,
	})
	if err != nil {
		t.Fatalf("buildPlan 10 runs: %v", err)
	}
	// Widget: ceil(10×10 × 0.9702) = 98; component: ceil(5×20 ×
	// 0.9702) = 98. Total 196.
	if req, _ := leafQty(res.Root, 34); req != 196 {
		t.Errorf("mineral at 10 runs: got %d, want 196", req)
	}
	// Time at the factored rate: (600 + 2×300) × 0.68 = 816.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:               1,
		FacilityTimeFactor: 0.85 * 0.80,
	})
	if err != nil {
		t.Fatalf("buildPlan time: %v", err)
	}
	if res.TotalSeconds != 816 {
		t.Errorf("total seconds: got %d, want 816", res.TotalSeconds)
	}
}

func TestPlanMEOverridesAtConsumingNode(t *testing.T) {
	// ME 10 on the Widget blueprint: component need ceil(2×0.9)=2,
	// mineral need ceil(10×0.9)=9, component branch unchanged.
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs:       1,
		MEOverride: map[int64]int{2001: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.RequiredQty != 2 {
		t.Errorf("component required: got %d, want 2 (ceil of 1.8)", comp.RequiredQty)
	}
	if req, _ := leafQty(res.Root, 34); req != 19 { // 9 direct + 10 under component
		t.Errorf("mineral required: got %d, want 19", req)
	}

	// ME 10 on the Component blueprint instead: its mineral need
	// becomes ceil(5×2×0.9)=9; the root's direct 10 stays.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:       1,
		MEOverride: map[int64]int{2002: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if req, _ := leafQty(res.Root, 34); req != 19 { // 10 direct + 9 under component
		t.Errorf("mineral required: got %d, want 19", req)
	}
	if comp := findPlanNode(res.Root, 1002); comp.ME != 10 || comp.MESource != "override" {
		t.Errorf("component ME/source: %d/%q, want 10/override", comp.ME, comp.MESource)
	}
}

func TestPlanOwnedBlueprintAutofillAndOverridePrecedence(t *testing.T) {
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs:  1,
		Owned: map[int64]OwnedBlueprint{2002: {ME: 8, TE: 12}},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.ME != 8 || comp.TE != 12 || comp.MESource != "owned" {
		t.Fatalf("component ME/TE/source: %d/%d/%q, want 8/12/owned", comp.ME, comp.TE, comp.MESource)
	}
	// Mineral under component: ceil(5×2×0.92) = 10; time with TE:
	// ceil(300×2×0.88) = 528.
	if comp.TimeSeconds != 528 {
		t.Errorf("component time: got %d, want 528", comp.TimeSeconds)
	}

	// An explicit ME override beats the owned copy's ME but keeps
	// its TE (same physical blueprint).
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:       1,
		Owned:      map[int64]OwnedBlueprint{2002: {ME: 8, TE: 12}},
		MEOverride: map[int64]int{2002: 3},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp = findPlanNode(res.Root, 1002)
	if comp.ME != 3 || comp.TE != 12 || comp.MESource != "override" {
		t.Errorf("override: ME/TE/source %d/%d/%q, want 3/12/override", comp.ME, comp.TE, comp.MESource)
	}
}

func TestPlanStockpileSingleAllocation(t *testing.T) {
	// 15 Mineral on hand: the root's own line (materials walk in
	// type-ID order) claims 10, the component's line gets the
	// remaining 5 — the stockpile is never promised twice.
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs:  1,
		Stock: map[int64]int64{34: 15, 1002: 1},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp := findPlanNode(res.Root, 1002)
	if comp.Have != 1 || comp.Shortfall != 1 || comp.Runs != 1 {
		t.Errorf("component: have %d shortfall %d runs %d, want 1/1/1", comp.Have, comp.Shortfall, comp.Runs)
	}
	totalHave := int64(0)
	Walk(res.Root, func(n *Node) { totalHave += n.Have })
	if totalHave != 16 { // 15 mineral + 1 component, never more
		t.Errorf("allocated %d units from a 16-unit stockpile set", totalHave)
	}
	if req, buy := leafQty(res.Root, 34); req != 15 || buy != 0 {
		t.Errorf("mineral: required %d to-buy %d, want 15/0 (10 at the root, 5 under the single component run the stock leaves; all covered)", req, buy)
	}

	// Fully stocked component: no runs, no subtree expansion.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:  1,
		Stock: map[int64]int64{1002: 2},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	comp = findPlanNode(res.Root, 1002)
	if comp.Blueprint != nil || comp.Shortfall != 0 || len(comp.Children) != 0 {
		t.Errorf("stocked component should be an unexpanded have-leaf: %+v", comp)
	}
}

func TestPlanWholeRunChildProduction(t *testing.T) {
	// Bulk Thing needs 250 Bulk Parts; the part blueprint makes
	// 100 per run, so 3 runs / 300 produced / 50 surplus.
	res, err := Build(fixtureRecipes(), 1006, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	part := findPlanNode(res.Root, 1005)
	if part == nil || part.Runs != 3 || part.ProducedQty != 300 || part.SurplusQty != 50 {
		t.Fatalf("bulk part node: %+v, want 3 runs / 300 / 50 surplus", part)
	}
	// Each part run eats 1 Mineral (base-1 floor holds at 3).
	if req, _ := leafQty(res.Root, 34); req != 3 {
		t.Errorf("mineral: got %d, want 3", req)
	}

	// Solo at 100 runs, ME 10: base-1 floor keeps the need at 100.
	res, err = Build(fixtureRecipes(), 1009, Input{
		Runs:       100,
		MEOverride: map[int64]int{2008: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if req, _ := leafQty(res.Root, 34); req != 100 {
		t.Errorf("base-1 floor: got %d, want 100", req)
	}
}

func TestPlanCycleGuard(t *testing.T) {
	res, err := Build(fixtureRecipes(), 1007, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	// Root A builds B; B's own A requirement loops back and must
	// come out as a flagged buy leaf, not infinite recursion.
	var cyc *Node
	Walk(res.Root, func(n *Node) {
		if n.Cycle {
			cyc = n
		}
	})
	if cyc == nil || cyc.TypeID != 1007 || cyc.Blueprint != nil {
		t.Fatalf("cycle node: %+v, want a flagged 1007 buy leaf", cyc)
	}
}

func TestPlanNotManufacturable(t *testing.T) {
	if _, err := Build(fixtureRecipes(), 424242, Input{Runs: 1}); err == nil {
		t.Error("unknown product should error")
	}
}

// Capital-scale source: 40 components x 15 sub-assemblies x 8
// leaves mirrors a deep capital tree (shared leaves expand
// under every parent, like shared minerals). 5441 nodes must
// clear the engine cap so caps auto-build on first load.
func capitalScaleSource() fixturePlannerSource {
	src := fixturePlannerSource{}
	const rootID = 10001
	rootMats := make([]Material, 0, 40)
	for i := 0; i < 40; i++ {
		compID := int64(11000 + i)
		rootMats = append(rootMats, Material{TypeID: compID, Quantity: 1})
		subMats := make([]Material, 0, 15)
		for j := 0; j < 15; j++ {
			subID := int64(12000 + i*15 + j)
			subMats = append(subMats, Material{TypeID: subID, Quantity: 1})
			leafMats := make([]Material, 0, 8)
			for k := 0; k < 8; k++ {
				leafMats = append(leafMats, Material{TypeID: int64(34 + k%4), Quantity: 10})
			}
			src[subID] = &Blueprint{BlueprintTypeID: 22000 + int64(i*15+j), ProductTypeID: subID,
				ProductQuantity: 1, TimeSeconds: 60, Materials: leafMats}
		}
		src[compID] = &Blueprint{BlueprintTypeID: 21000 + int64(i), ProductTypeID: compID,
			ProductQuantity: 1, TimeSeconds: 600, Materials: subMats}
	}
	src[rootID] = &Blueprint{BlueprintTypeID: 20001, ProductTypeID: rootID,
		ProductQuantity: 1, TimeSeconds: 3600, Materials: rootMats}
	return src
}

func TestPlanCapitalScaleBuilds(t *testing.T) {
	res, err := Build(capitalScaleSource(), 10001, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.TooLarge {
		t.Fatal("capital-scale tree tripped the node cap")
	}
	if res.Root == nil {
		t.Fatal("capital-scale tree has no root")
	}
	nodes := 0
	Walk(res.Root, func(n *Node) { nodes++ })
	if nodes != 5441 {
		t.Errorf("capital-scale nodes: got %d, want 5441", nodes)
	}
	if len(res.Shopping) == 0 {
		t.Error("capital-scale tree aggregated no shopping")
	}
}

func TestPlanAbsurdScaleStillTripsCap(t *testing.T) {
	src := capitalScaleSource()
	root := src[10001]
	wide := make([]Material, 0, 400)
	for i := 0; i < 400; i++ {
		id := int64(50000 + i)
		wide = append(wide, Material{TypeID: id, Quantity: 1})
		src[id] = &Blueprint{BlueprintTypeID: 60000 + int64(i), ProductTypeID: id,
			ProductQuantity: 1, TimeSeconds: 1, Materials: root.Materials}
	}
	src[10001] = &Blueprint{BlueprintTypeID: 20001, ProductTypeID: 10001,
		ProductQuantity: 1, TimeSeconds: 1, Materials: wide}
	res, err := Build(src, 10001, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.TooLarge {
		t.Error("absurd-scale tree should still trip the node cap")
	}
}

func TestPlanPriceHonesty(t *testing.T) {
	// No prices at all: costs stay unknown, quantities don't.
	res, err := Build(fixtureRecipes(), 1001, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.UnpricedLines != 1 || res.Shopping[0].PriceKnown {
		t.Errorf("unpriced shopping line not flagged: %+v", res.Shopping)
	}
	if res.Root.CostComplete {
		t.Error("root cost must be incomplete when a price is missing")
	}

	// Adjusted price stands in when the average is absent.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:   1,
		Prices: map[int64]esi.MarketPrice{34: {TypeID: 34, AdjustedPrice: 3.5}},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.Root.CostComplete || res.Root.LineCost != 70 { // 20 × 3.5
		t.Errorf("root line cost: got %v (complete %v), want 70/true", res.Root.LineCost, res.Root.CostComplete)
	}
}

func TestPlanJobFees(t *testing.T) {
	// Mineral 10/unit guide, 12 adjusted; component 100
	// adjusted; index 5%. Root installs 2 components + 10
	// mineral: (2×100 + 10×12) × 0.05 = 16.
	prices := map[int64]esi.MarketPrice{34: {TypeID: 34, AveragePrice: 10}}
	adjusted := map[int64]float64{34: 12, 1002: 100}
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs: 1, Prices: prices, Adjusted: adjusted, CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.Root.JobKnown || res.Root.JobCost != 16 {
		t.Errorf("root fee: got %v (known %v), want 16/true", res.Root.JobCost, res.Root.JobKnown)
	}
	// Component runs twice, 10 mineral installed: 10×12×0.05.
	comp := findPlanNode(res.Root, 1002)
	if !comp.JobKnown || comp.JobCost != 6 {
		t.Errorf("component fee: got %v (known %v), want 6/true", comp.JobCost, comp.JobKnown)
	}
	// Leaves run no job.
	Walk(res.Root, func(n *Node) {
		if n.Blueprint == nil && (!n.JobKnown || n.JobCost != 0) {
			t.Errorf("leaf %d fee: got %v (known %v), want 0/true", n.TypeID, n.JobCost, n.JobKnown)
		}
	})
}

func TestPlanJobFeesUnknown(t *testing.T) {
	// No adjusted map: the fee stays unknown, never a zero.
	res, err := Build(fixtureRecipes(), 1001, Input{
		Runs:      1,
		Prices:    map[int64]esi.MarketPrice{34: {TypeID: 34, AveragePrice: 10}},
		CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.Root.JobKnown || res.Root.JobCost != 0 {
		t.Errorf("root fee: got %v (known %v), want 0/false", res.Root.JobCost, res.Root.JobKnown)
	}
	// No index at all: unknown too, even with adjusted prices.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs:     1,
		Prices:   map[int64]esi.MarketPrice{34: {TypeID: 34, AveragePrice: 10}},
		Adjusted: map[int64]float64{34: 12, 1002: 100},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.Root.JobKnown {
		t.Error("root fee known without an index")
	}
}

func TestPlanResearchSaving(t *testing.T) {
	// Root at ME 0: the direct mineral (10 at 10/unit) drops to
	// 9 at ME10, saving 10; the 2 components already round to 2
	// either way. The component branch saves the same 10 under
	// its own runs.
	prices := map[int64]esi.MarketPrice{34: {TypeID: 34, AveragePrice: 10}}
	res, err := Build(fixtureRecipes(), 1001, Input{Runs: 1, Prices: prices})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.Root.ResearchKnown || res.Root.ResearchSaving != 10 {
		t.Errorf("root saving: got %v (known %v), want 10/true", res.Root.ResearchSaving, res.Root.ResearchKnown)
	}
	comp := findPlanNode(res.Root, 1002)
	if !comp.ResearchKnown || comp.ResearchSaving != 10 {
		t.Errorf("component saving: got %v (known %v), want 10/true", comp.ResearchSaving, comp.ResearchKnown)
	}
	// Already at ME10: nothing left to save, still known.
	res, err = Build(fixtureRecipes(), 1001, Input{
		Runs: 1, Prices: prices, MEOverride: map[int64]int{2001: 10},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if !res.Root.ResearchKnown || res.Root.ResearchSaving != 0 {
		t.Errorf("researched root saving: got %v (known %v), want 0/true", res.Root.ResearchSaving, res.Root.ResearchKnown)
	}
	// No prices: the saving is unknown, never a zero.
	res, err = Build(fixtureRecipes(), 1001, Input{Runs: 1})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	if res.Root.ResearchKnown {
		t.Error("root saving known without prices")
	}
}
