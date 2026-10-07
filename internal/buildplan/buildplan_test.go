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
