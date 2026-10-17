package buildplanner

// Price-source tests: the resolved price map behind every plan
// figure (the house order), and a supercarrier-shaped payload
// priced from it -- the P0-2 regression. The fixture product
// trades only by contract, like a supercarrier: its guide row
// carries no average, only CCP's adjusted figure, so the old
// guide-fed planner used that one stale number as the hull's
// buy price, its sell value and its verdict input at once.

import (
	"testing"

	"evesynapse/internal/buildplan"
)

// planFossilAdjusted is the real adjusted price from the Hel's
// guide row (type 22852): the number the old planner priced
// the hull at. It may base job fees (CCP defines the fee base
// on adjusted prices); it must never be a plan price.
const planFossilAdjusted = 18893271495.42

// planCarrierRecipes is a supercarrier-shaped tree: a hull
// built from a manufactured component and a raw mineral.
func planCarrierRecipes() buildplan.Source {
	return planTestSource{
		5001: {BlueprintTypeID: 6001, ProductTypeID: 5001, ProductQuantity: 1, TimeSeconds: 3600, Materials: []buildplan.Material{
			{TypeID: 5002, Quantity: 2}, {TypeID: 34, Quantity: 100},
		}},
		5002: {BlueprintTypeID: 6002, ProductTypeID: 5002, ProductQuantity: 1, TimeSeconds: 600, Materials: []buildplan.Material{
			{TypeID: 34, Quantity: 50},
		}},
	}
}

// planCarrierSources is the stored data behind the carrier
// fixture: the mineral and the component trade in The Forge
// (typical sells on file), the hull trades only by contract
// (a stored estimated sell, no typical).
func planCarrierSources() (ids []int64, typicals, estSells map[int64]float64) {
	ids = []int64{5001, 5002, 34}
	typicals = map[int64]float64{34: 6.5, 5002: 900}
	estSells = map[int64]float64{5001: 43_000_000_000}
	return ids, typicals, estSells
}

// TestResolvePlanPricesHouseOrder pins the resolution order:
// Forge typical first, stored estimated sell behind it, and
// nothing behind that -- a type neither stored source knows
// is unpriced. (The guide is not an input to resolution at
// all: no guide figure, average or adjusted, can reach a plan
// price.) Resolved prices ride in AveragePrice with the
// adjusted slot zeroed, so UnitPrice can never fall through.
func TestResolvePlanPricesHouseOrder(t *testing.T) {
	ids := []int64{1, 2, 3, 4, 5}
	typicals := map[int64]float64{1: 10, 2: 0, 4: 7}
	estSells := map[int64]float64{1: 99, 2: 42, 3: 0}
	out := resolvePlanPrices(ids, typicals, estSells)
	want := map[int64]float64{1: 10, 2: 42, 4: 7}
	if len(out) != len(want) {
		t.Fatalf("resolved %d prices, want %d: %v", len(out), len(want), out)
	}
	for id, price := range want {
		p, ok := out[id]
		if !ok {
			t.Errorf("type %d missing from the resolved map", id)
			continue
		}
		if p.AveragePrice != price || p.AdjustedPrice != 0 {
			t.Errorf("type %d resolved = %+v, want average %v and no adjusted", id, p, price)
		}
		if got, known := buildplan.UnitPrice(out, id); !known || got != price {
			t.Errorf("UnitPrice(%d) = %v/%v, want %v/true", id, got, known, price)
		}
	}
	// 3: an estimated sell of zero is no estimate. 5: neither
	// source has heard of it. Both stay unpriced.
	for _, id := range []int64{3, 5} {
		if _, ok := out[id]; ok {
			t.Errorf("type %d resolved, want it left unpriced", id)
		}
		if _, known := buildplan.UnitPrice(out, id); known {
			t.Errorf("UnitPrice(%d) known, want unknown", id)
		}
	}
}

// TestPlanCarrierValueIsStoredEstSell is the Hel regression:
// priced from the resolved map, the plan's market value for a
// contract-only hull is the stored estimated sell -- the same
// figure the item page renders from true_values -- and the
// guide's adjusted fossil appears in no price role. (The item
// page reads the stored estimated sell verbatim; the planner
// resolving to that same stored figure is what keeps the two
// pages from disagreeing about what the product is worth.)
func TestPlanCarrierValueIsStoredEstSell(t *testing.T) {
	ids, typicals, estSells := planCarrierSources()
	prices := resolvePlanPrices(ids, typicals, estSells)
	if got := prices[5001].AveragePrice; got != estSells[5001] {
		t.Fatalf("resolved hull price = %v, want the stored estimated sell %v", got, estSells[5001])
	}
	res, err := buildplan.Build(planCarrierRecipes(), 5001, buildplan.Input{
		Runs: 1, Prices: prices,
		Adjusted:  map[int64]float64{34: 5.5, 5002: 950, 5001: planFossilAdjusted},
		CostIndex: 0.001,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, 5001, prices, unitOf, 1, 1, nil, planFixtureName, nil)

	const wantValue = 4_300_000_000_000 // 43B ISK in cents
	for _, preset := range []planPresetTotals{p.Full, p.Optimized} {
		if preset.Value != wantValue {
			t.Errorf("preset value = %d, want %d (the stored estimated sell)", preset.Value, wantValue)
		}
		if preset.Value == planCents(planFossilAdjusted) {
			t.Errorf("preset value is the guide's adjusted fossil %v", planFossilAdjusted)
		}
		if !preset.Known {
			t.Error("preset value unknown, want known from the estimated sell")
		}
	}
	// Building wins at these figures: the component builds for
	// 325 (50 mineral at the 6.5 typical) against a 900 buy, and
	// the hull's materials total 1,300 ISK against a 43B buy, so
	// the optimized preset builds the root instead of buying it
	// at the fossil -- or at any price on file.
	if p.Optimized.RootBought {
		t.Error("optimized preset bought the root, want it built")
	}
	if p.Optimized.Materials != 130_000 {
		t.Errorf("optimized materials = %d, want 130000 (200 mineral at the 6.5 typical, in cents)", p.Optimized.Materials)
	}
	if !p.Optimized.ProfKnown || p.Optimized.Profit <= 0 {
		t.Errorf("optimized profit = %d (known %v), want a real positive margin against the estimated sell",
			p.Optimized.Profit, p.Optimized.ProfKnown)
	}
}

// TestPlanLeafPricesAtForgeTypical pins the input side: a
// leaf's shopping line prices at its Forge typical sell, the
// price a builder actually pays in Jita -- not at any cluster
// statistic. The fixture mineral's typical (6.5) deliberately
// differs from the averages such a statistic would carry.
func TestPlanLeafPricesAtForgeTypical(t *testing.T) {
	ids, typicals, estSells := planCarrierSources()
	prices := resolvePlanPrices(ids, typicals, estSells)
	res, err := buildplan.Build(planCarrierRecipes(), 5001, buildplan.Input{
		Runs: 1, Prices: prices,
		Adjusted:  map[int64]float64{34: 5.5, 5002: 950, 5001: planFossilAdjusted},
		CostIndex: 0.001,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, 5001, prices, unitOf, 1, 1, nil, planFixtureName, nil)
	if len(p.Full.Shopping) != 1 {
		t.Fatalf("full shopping lines = %d, want 1 (the mineral): %+v", len(p.Full.Shopping), p.Full.Shopping)
	}
	line := p.Full.Shopping[0]
	if line.TypeID != 34 || line.Qty != 200 {
		t.Fatalf("shopping line = type %d qty %d, want 34 x 200", line.TypeID, line.Qty)
	}
	if !line.Known || line.Unit != 650 || line.Cost != 130_000 {
		t.Errorf("mineral line = unit %d cost %d known %v, want 650/130000/true (the 6.5 typical)",
			line.Unit, line.Cost, line.Known)
	}
}

// TestPlanUnpricedLeafStaysUnpriced: a type no stored source
// knows must surface as no price data -- an incomplete plan
// that says so -- never a zero-priced line or a statistic.
func TestPlanUnpricedLeafStaysUnpriced(t *testing.T) {
	prices := resolvePlanPrices([]int64{5001, 5002, 34},
		map[int64]float64{5002: 900}, map[int64]float64{5001: 43_000_000_000})
	res, err := buildplan.Build(planCarrierRecipes(), 5001, buildplan.Input{
		Runs: 1, Prices: prices,
		Adjusted:  map[int64]float64{34: 5.5, 5002: 950, 5001: planFossilAdjusted},
		CostIndex: 0.001,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Root.CostComplete {
		t.Error("root cost complete with an unpriced mineral, want incomplete")
	}
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, 5001, prices, unitOf, 1, 1, nil, planFixtureName, nil)
	for _, line := range p.Full.Shopping {
		if line.TypeID == 34 && line.Known {
			t.Errorf("unpriced mineral carries a price: %+v", line)
		}
	}
}
