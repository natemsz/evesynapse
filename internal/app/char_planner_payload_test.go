package app

// Payload tests: preset economics, the bought/built split, and
// the slot-aware schedule around a hand-built engine result.
// These pin the server side of the client cross-check.

import (
	"fmt"
	"testing"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// planTestSource is a map-backed recipe source for payload tests.
type planTestSource map[int64]*buildplan.Blueprint

func (s planTestSource) BlueprintForProduct(id int64) (*buildplan.Blueprint, bool) {
	b, ok := s[id]
	return b, ok
}

// planFixtureRecipes is a two-level tree: the root builds from
// one component (own blueprint, 300s at TE0) and mineral, each
// priced so optimized buys nothing.
func planFixtureRecipes() buildplan.Source {
	return planTestSource{
		1001: {BlueprintTypeID: 2001, ProductTypeID: 1001, ProductQuantity: 1, TimeSeconds: 600, Materials: []buildplan.Material{
			{TypeID: 1002, Quantity: 1}, {TypeID: 34, Quantity: 10},
		}},
		1002: {BlueprintTypeID: 2002, ProductTypeID: 1002, ProductQuantity: 1, TimeSeconds: 300, Materials: []buildplan.Material{
			{TypeID: 34, Quantity: 5},
		}},
	}
}

func planFixtureName(id int64) string { return fmt.Sprintf("Type %d", id) }

func planFixturePrices() map[int64]esi.MarketPrice {
	return map[int64]esi.MarketPrice{
		34:   {TypeID: 34, AveragePrice: 10},
		1002: {TypeID: 1002, AveragePrice: 500},
	}
}

func planFixtureAdjusted() map[int64]float64 { return map[int64]float64{34: 12, 1002: 480} }

// TestPlanPayloadPresets: optimized matches full when building
// saves everywhere; buy-components prunes the subtree; every
// preset's bought+built equals its materials.
func TestPlanPayloadPresets(t *testing.T) {
	res, err := buildplan.Build(planFixtureRecipes(), 1001, buildplan.Input{
		Runs: 1, Prices: planFixturePrices(),
		Adjusted: planFixtureAdjusted(), CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(planFixturePrices(), id) }
	p := buildPlanPayload(res, 1001, planFixturePrices(), unitOf, 1, 2, nil, planFixtureName, nil)
	if len(p.Root.Kids) != 2 {
		t.Fatalf("root kids = %d, want 2", len(p.Root.Kids))
	}
	// Optimized == full here: the component costs 50 to build
	// against 500 to buy. Materials 150 (50 under the
	// component + 100 direct). Fees are charged on the estimated
	// item value at index 5% plus the 4% SCC surcharge (no
	// facility tax given): (480 + 120) × 0.09 = 54 at the root
	// plus 60 × 0.09 = 5.40 at the component.
	for _, preset := range []planPresetTotals{p.Full, p.Optimized} {
		if preset.Materials != 15000 || preset.Fees != 5940 || preset.Landed != 20940 {
			t.Errorf("preset money = %v/%v/%v, want 15000/5940/20940",
				preset.Materials, preset.Fees, preset.Landed)
		}
		if !preset.FeesKnown || !preset.Complete {
			t.Errorf("preset known = fees %v complete %v, want true/true", preset.FeesKnown, preset.Complete)
		}
		if preset.Bought+preset.Built != preset.Materials {
			t.Errorf("bought %v + built %v != materials %v", preset.Bought, preset.Built, preset.Materials)
		}
		if preset.Bought != 0 || preset.Built != 15000 {
			t.Errorf("bought/built = %v/%v, want 0/15000 (everything built)", preset.Bought, preset.Built)
		}
	}
	// Buy-components: the root still builds (100 direct mineral
	// + fee), the component is bought at 500.
	b := p.BuyComp
	if b.Materials != 60000 || b.Fees != 5400 || b.Landed != 65400 {
		t.Errorf("buycomp money = %v/%v/%v, want 60000/5400/65400", b.Materials, b.Fees, b.Landed)
	}
	if b.Bought != 50000 || b.Built != 10000 {
		t.Errorf("buycomp bought/built = %v/%v, want 50000/10000", b.Bought, b.Built)
	}
	if len(b.Shopping) != 2 {
		t.Fatalf("buycomp shopping lines = %d, want 2", len(b.Shopping))
	}
}

// TestPlanPayloadSchedule: two slots run the component, then
// the root; the critical path covers both jobs.
func TestPlanPayloadSchedule(t *testing.T) {
	res, err := buildplan.Build(planFixtureRecipes(), 1001, buildplan.Input{
		Runs: 1, Prices: planFixturePrices(),
		Adjusted: planFixtureAdjusted(), CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(planFixturePrices(), id) }
	p := buildPlanPayload(res, 1001, planFixturePrices(), unitOf, 1, 2, nil, planFixtureName, nil)
	if p.Optimized.Elapsed != 900 {
		t.Errorf("elapsed = %v, want 900 (300 then 600)", p.Optimized.Elapsed)
	}
	if len(p.Optimized.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(p.Optimized.Jobs))
	}
	var crit int
	for _, j := range p.Optimized.Jobs {
		if j.Critical {
			crit++
		}
		if j.Start < 0 || j.End <= j.Start {
			t.Errorf("job %+v has a non-positive span", j)
		}
	}
	if crit != 2 {
		t.Errorf("critical jobs = %d, want 2 (the whole chain)", crit)
	}
	if p.BuyComp.Elapsed != 600 || len(p.BuyComp.Jobs) != 1 {
		t.Errorf("buycomp elapsed/jobs = %v/%d, want 600/1", p.BuyComp.Elapsed, len(p.BuyComp.Jobs))
	}
}

// TestPlanShoppingKnownNodeCoversPricedLines locks the contract
// the client's shopping lookup relies on. The engine prices
// shortfalls only, so a stock-covered leaf carries no price while
// a deeper twin of the same type can be priced; the client must
// resolve the priced twin, not the first twin. Stock here covers
// the direct Tritanium (10 of 12) but not the nested shortfall
// (5 needed, 2 left), which is the shape that once tripped the
// client/server cross-check.
func TestPlanShoppingKnownNodeCoversPricedLines(t *testing.T) {
	prices := planFixturePrices()
	res, err := buildplan.Build(planFixtureRecipes(), 1001, buildplan.Input{
		Runs: 1, Prices: prices, Adjusted: planFixtureAdjusted(),
		CostIndex: 0.05, Stock: map[int64]int64{34: 12},
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, 1001, prices, unitOf, 1, 2, nil, planFixtureName, nil)

	known := map[int64]int64{}
	var walk func(n *planNodeDTO)
	walk = func(n *planNodeDTO) {
		if n == nil {
			return
		}
		if n.Known {
			if _, ok := known[n.ID]; !ok {
				known[n.ID] = n.Unit
			}
		}
		for _, k := range n.Kids {
			walk(k)
		}
	}
	walk(p.Root)
	// Shape guard: Tritanium must appear twice with mixed
	// pricing, or this test no longer exercises the hazard.
	seen, priced := 0, 0
	var recount func(n *planNodeDTO)
	recount = func(n *planNodeDTO) {
		if n == nil {
			return
		}
		if n.ID == 34 {
			seen++
			if n.Known {
				priced++
			}
		}
		for _, k := range n.Kids {
			recount(k)
		}
	}
	recount(p.Root)
	if seen != 2 || priced != 1 {
		t.Fatalf("tritanium twins = %d nodes %d priced, want 2/1 (covered direct, short nested)", seen, priced)
	}
	// Contract: every priced shopping line resolves to a priced
	// tree node with the same unit.
	for _, preset := range []planPresetTotals{p.Full, p.Optimized, p.BuyComp} {
		for _, line := range preset.Shopping {
			if !line.Known {
				continue
			}
			u, ok := known[line.TypeID]
			if !ok || u != line.Unit {
				t.Errorf("shopping %+v resolves to node unit %v ok=%v, want a priced twin", line, u, ok)
			}
		}
	}
	// The nested shortfall is the only Tritanium line here.
	if len(p.Optimized.Shopping) != 1 || p.Optimized.Shopping[0].Qty != 3 {
		t.Errorf("optimized shopping = %+v, want one Tritanium line of 3", p.Optimized.Shopping)
	}
}

// TestPlanShoppingExcludesRoot: when optimized buys the finished
// product outright (its price undercuts the build cost), the
// product itself must not appear as a shopping line — the buy
// verdict lives in the header, and the Avatar is not an input to
// its own build. The bought cost still accrues to the money
// totals; only the aggregation skips the root, on both sides of
// the client cross-check.
func TestPlanShoppingExcludesRoot(t *testing.T) {
	prices := planFixturePrices()
	prices[1001] = esi.MarketPrice{TypeID: 1001, AveragePrice: 50}
	res, err := buildplan.Build(planFixtureRecipes(), 1001, buildplan.Input{
		Runs: 1, Prices: prices, Adjusted: planFixtureAdjusted(),
		CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, 1001, prices, unitOf, 1, 2, nil, planFixtureName, nil)

	b := p.Optimized
	for _, line := range b.Shopping {
		if line.TypeID == 1001 {
			t.Errorf("optimized shopping contains the finished product: %+v", line)
		}
	}
	if len(b.Shopping) != 0 {
		t.Errorf("optimized shopping = %+v, want empty (root bought outright)", b.Shopping)
	}
	// The money still counts the bought hull: 1 x 50.
	if b.Materials != 5000 || b.Bought != 5000 || b.Landed != 5000 {
		t.Errorf("optimized money = %v/%v/%v, want 5000/5000/5000", b.Materials, b.Bought, b.Landed)
	}
	if b.Profit != 0 || !b.ProfKnown {
		t.Errorf("optimized profit = %v known=%v, want 0/true (buying at market)", b.Profit, b.ProfKnown)
	}
	// Full build is untouched: the mineral lines stay.
	if len(p.Full.Shopping) != 1 || p.Full.Shopping[0].TypeID != 34 {
		t.Errorf("full shopping = %+v, want the one Tritanium line", p.Full.Shopping)
	}
}

// TestPlanTimeFactorAndSlots: manual values win; the defaults
// degrade honestly without skill data.
func TestPlanTimeFactorAndSlots(t *testing.T) {
	ids := map[string]int64{"Industry": 3380, "Advanced Industry": 3390}
	f, note := planTimeFactor("0.5", ids, plannerScopeData{}, nil)
	if f != 0.5 || note == "" {
		t.Errorf("manual factor = %v %q, want 0.5 + note", f, note)
	}
	f, _ = planTimeFactor("junk", ids, plannerScopeData{}, nil)
	if f != 1 {
		t.Errorf("default factor = %v, want 1 without skills", f)
	}
	f, _ = planTimeFactor("", ids, plannerScopeData{
		Skills: map[int64]map[int64]int{7: {3380: 5, 3390: 4}},
	}, []db.Character{{CharacterID: 7}})
	want := (1 - 0.04*5) * (1 - 0.03*4)
	if f < want-1e-12 || f > want+1e-12 {
		t.Errorf("skilled factor = %v, want %v", f, want)
	}
	s, _ := planSlots("3", ids, plannerScopeData{}, nil)
	if s != 3 {
		t.Errorf("manual slots = %d, want 3", s)
	}
	s, _ = planSlots("", map[string]int64{}, plannerScopeData{}, nil)
	if s != 1 {
		t.Errorf("default slots = %d, want 1 without skills", s)
	}
}

// TestPlanPayloadOrigins: leaf origins ride the payload — a PI
// commodity by name (tier from the PI planner's own table), a
// reaction product by SDE group, ordinary minerals unclassified,
// buildable nodes unclassified.
func TestPlanPayloadOrigins(t *testing.T) {
	res, err := buildplan.Build(planFixtureRecipes(), 1001, buildplan.Input{
		Runs: 1, Prices: planFixturePrices(),
		Adjusted: planFixtureAdjusted(), CostIndex: 0.05,
	})
	if err != nil {
		t.Fatalf("buildPlan: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(planFixturePrices(), id) }
	nameOf := func(id int64) string {
		if id == 34 {
			return "Biocells" // a PI commodity name
		}
		return planFixtureName(id)
	}
	originOf := planOriginClassifier(map[int64]string{1002: "Advanced Materials"})
	p := buildPlanPayload(res, 1001, planFixturePrices(), unitOf, 1, 2, nil, nameOf, originOf)
	var mineral, comp *planNodeDTO
	for _, k := range p.Root.Kids {
		switch k.ID {
		case 34:
			mineral = k
		case 1002:
			comp = k
		}
	}
	if mineral == nil || comp == nil {
		t.Fatalf("fixture kids missing: %v", p.Root.Kids)
	}
	if mineral.Origin != "pi" || mineral.OriginTier == "" {
		t.Errorf("mineral-named-Biocells origin = %q/%q, want pi/<tier>", mineral.Origin, mineral.OriginTier)
	}
	if comp.Origin != "" {
		t.Errorf("buildable component origin = %q, want empty (it has a blueprint)", comp.Origin)
	}
	// The same classifier marks the component when it appears as
	// an unbuildable leaf: covered by classifying its group
	// directly.
	if o, _ := originOf(1002, "Type 1002"); o != "reaction" {
		t.Errorf("Advanced Materials origin = %q, want reaction", o)
	}
	if o, _ := originOf(34, "Tritanium"); o != "" {
		t.Errorf("plain mineral origin = %q, want empty", o)
	}
}
