package buildplan

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// UnitCoster fixtures: a hand-checkable chain (leaf minerals 34
// and 35, a component, a multi-output component, a ship root)
// plus the package's capital-scale source for the decomposition
// test. Prices: 34 -> 10, 35 -> 100; adjusted 34 -> 8, 35 -> 90.
// ---------------------------------------------------------------------------

func tvSource() fixturePlannerSource {
	return fixturePlannerSource{
		5001: {BlueprintTypeID: 6001, ProductTypeID: 5001, ProductQuantity: 1, TimeSeconds: 300,
			Materials: []Material{{TypeID: 34, Quantity: 10}, {TypeID: 35, Quantity: 2}}},
		5002: {BlueprintTypeID: 6002, ProductTypeID: 5002, ProductQuantity: 5, TimeSeconds: 60,
			Materials: []Material{{TypeID: 34, Quantity: 10}}},
		5003: {BlueprintTypeID: 6003, ProductTypeID: 5003, ProductQuantity: 1, TimeSeconds: 3600,
			Materials: []Material{{TypeID: 5001, Quantity: 4}, {TypeID: 34, Quantity: 100}}},
		5004: {BlueprintTypeID: 6004, ProductTypeID: 5004, ProductQuantity: 1, TimeSeconds: 60,
			Materials: []Material{{TypeID: 34, Quantity: 30}}},
		5005: {BlueprintTypeID: 6005, ProductTypeID: 5005, ProductQuantity: 1, TimeSeconds: 60,
			Materials: []Material{{TypeID: 9999, Quantity: 1}}},
	}
}

func tvLeafPrice(prices map[int64]float64) func(int64) (float64, bool) {
	return func(typeID int64) (float64, bool) {
		p, ok := prices[typeID]
		return p, ok
	}
}

func tvAdjusted() map[int64]float64 {
	return map[int64]float64{34: 8, 35: 90, 9999: 1}
}

func TestUnitCostArithmetic(t *testing.T) {
	leaf := tvLeafPrice(map[int64]float64{34: 10, 35: 100})
	profile := CostProfile{RootME: 2, ComponentME: 10}
	c := NewUnitCoster(tvSource(), leaf, tvAdjusted(), profile)

	// Component 5001 at ME10: ceil(10 x .9)=9 of 34 (90) +
	// ceil(2 x .9)=2 of 35 (200) = 290.
	if got, ok := c.UnitCost(5001); !ok || got != 290 {
		t.Errorf("UnitCost(5001) = %v, %v; want 290, true", got, ok)
	}
	// Multi-output 5002: one run makes 5 from 9 of 34 -> 90/5.
	if got, ok := c.UnitCost(5002); !ok || got != 18 {
		t.Errorf("UnitCost(5002) = %v, %v; want 18, true", got, ok)
	}
	// Root 5003 at hull ME2: ceil(4 x .98)=4 components (1160)
	// + ceil(100 x .98)=98 of 34 (980) = 2140.
	if got, ok := c.RootCost(5003); !ok || got != 2140 {
		t.Errorf("RootCost(5003) = %v, %v; want 2140, true", got, ok)
	}
	// The same hull costed as a component (ME10) is cheaper:
	// 4 components (1160) + 90 of 34 (900) = 2060 -- the root
	// ME is the root's alone.
	if got, ok := c.UnitCost(5003); !ok || got != 2060 {
		t.Errorf("UnitCost(5003) = %v, %v; want 2060, true", got, ok)
	}
	// A leaf prices at its leaf price, root or not.
	if got, ok := c.RootCost(34); !ok || got != 10 {
		t.Errorf("RootCost(34) = %v, %v; want 10, true", got, ok)
	}
}

func TestUnitCostJobFee(t *testing.T) {
	leaf := tvLeafPrice(map[int64]float64{34: 10, 35: 100})
	// Index 0.05, no facility cost bonus, tax 0.25%: the fee on
	// 5001 is EIV (10x8 + 2x90 = 260) x (0.05 + 0.0025 + 0.04
	// SCC) = 260 x 0.0925 = 24.05 on top of the 290 materials.
	profile := CostProfile{RootME: 2, ComponentME: 10, CostIndex: 0.05, FacilityTax: 0.0025}
	c := NewUnitCoster(tvSource(), leaf, tvAdjusted(), profile)
	got, ok := c.UnitCost(5001)
	if !ok || math.Abs(got-314.05) > 0.001 {
		t.Errorf("UnitCost(5001) with fee = %v, %v; want 314.05, true", got, ok)
	}

	// A direct material without an adjusted price leaves the
	// fee out rather than inventing it: materials only.
	adjusted := tvAdjusted()
	delete(adjusted, 35)
	c2 := NewUnitCoster(tvSource(), leaf, adjusted, profile)
	if got, ok := c2.UnitCost(5001); !ok || got != 290 {
		t.Errorf("UnitCost(5001) fee-unknown = %v, %v; want 290, true", got, ok)
	}
}

func TestUnitCostFacilityFactor(t *testing.T) {
	leaf := tvLeafPrice(map[int64]float64{34: 10, 35: 100})
	plain := NewUnitCoster(tvSource(), leaf, tvAdjusted(), CostProfile{RootME: 2, ComponentME: 10})
	if got, ok := plain.UnitCost(5004); !ok || got != 270 {
		t.Fatalf("UnitCost(5004) unrigged = %v, %v; want 270, true", got, ok)
	}
	// A 5% facility material bonus: ceil(30 x .9 x .95) = 26.
	rigged := NewUnitCoster(tvSource(), leaf, tvAdjusted(), CostProfile{RootME: 2, ComponentME: 10, MatFactor: 0.95})
	if got, ok := rigged.UnitCost(5004); !ok || got != 260 {
		t.Errorf("UnitCost(5004) rigged = %v, %v; want 260, true", got, ok)
	}
}

func TestUnitCostUnknownLeafStaysUnknown(t *testing.T) {
	leaf := tvLeafPrice(map[int64]float64{34: 10, 35: 100}) // 9999 unpriced
	c := NewUnitCoster(tvSource(), leaf, tvAdjusted(), CostProfile{RootME: 2, ComponentME: 10})
	if _, ok := c.UnitCost(5005); ok {
		t.Error("UnitCost(5005) priced over an unpriced leaf; want unknown")
	}
	if _, ok := c.RootCost(5005); ok {
		t.Error("RootCost(5005) priced over an unpriced leaf; want unknown")
	}
	if _, ok := c.UnitCost(9999); ok {
		t.Error("UnitCost(9999) priced with no leaf price; want unknown")
	}
}

func TestUnitCostCyclePricesAsLeaf(t *testing.T) {
	// fixtureRecipes' 1007 <-> 1008 loop: the repeated type
	// prices at its leaf price and evaluation terminates.
	leaf := tvLeafPrice(map[int64]float64{1007: 50, 1008: 50})
	c := NewUnitCoster(fixtureRecipes(), leaf, nil, CostProfile{RootME: 2, ComponentME: 10})
	if got, ok := c.UnitCost(1007); !ok || got != 50 {
		t.Errorf("UnitCost(1007) = %v, %v; want 50, true", got, ok)
	}
}

func TestUnitCostDecomposesCapitalToBaseInputs(t *testing.T) {
	// The capital-scale source: root 10001 -> 40 components ->
	// 15 subcomponents each -> mineral lines only (34..37).
	// Every leaf the coster consults must be a base input (no
	// blueprint), and shared subtrees price exactly once.
	consulted := make(map[int64]int)
	leaf := func(typeID int64) (float64, bool) {
		consulted[typeID]++
		if typeID >= 34 && typeID <= 37 {
			return 5, true
		}
		return 0, false
	}
	c := NewUnitCoster(capitalScaleSource(), leaf, nil, CostProfile{RootME: 2, ComponentME: 10})
	got, ok := c.RootCost(10001)
	if !ok {
		t.Fatalf("RootCost(10001) unknown; consulted leaves: %v", consulted)
	}
	// Sub: 8 lines x ceil(10 x .9)=9 x 5 = 360. Component:
	// 15 subs x 360 = 5400. Root: 40 x 5400 = 216000.
	if got != 216000 {
		t.Errorf("RootCost(10001) = %v; want 216000", got)
	}
	for id, calls := range consulted {
		if id < 34 || id > 37 {
			t.Errorf("costed a non-base type as a leaf: %d", id)
		}
		if calls != 1 {
			t.Errorf("leaf %d priced %d times; memoization should make it 1", id, calls)
		}
	}
	if len(consulted) != 4 {
		t.Errorf("consulted %d leaf types; want the 4 minerals", len(consulted))
	}
}
