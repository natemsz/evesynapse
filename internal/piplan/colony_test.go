package piplan

import "testing"

// TestTemplateMatchesGameFormat holds the colony template to the
// format the game imports: a template the game exported reads back
// with its structures, links and routes, and writes out again with
// the same numbering and type ids; link costs follow the game's
// per-level formula.
func TestTemplateMatchesGameFormat(t *testing.T) {
	// Exported from the game (a Barren P3 factory, trimmed).
	raw := `{"CmdCtrLv": 5, "Cmt": "Factory", "Diam": 5820.0,
"L": [{"D": 2, "Lv": 0, "S": 3}, {"D": 1, "Lv": 1, "S": 3}],
"P": [{"H": 0, "La": 1.10907, "Lo": 1.53804, "S": 17392, "T": 2474},
{"H": 0, "La": 1.13032, "Lo": 1.53929, "S": 17392, "T": 2474},
{"H": 0, "La": 1.11954, "Lo": 1.54545, "S": null, "T": 2544}],
"Pln": 2016, "R": [{"P": [3, 2], "Q": 10, "T": 2327}, {"P": [1, 3], "Q": 3, "T": 17392}]}`
	c, err := ParseTemplate(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.PlanetType != "Barren" || c.RadiusKm != 2910 || len(c.Pins) != 3 || len(c.Links) != 2 || len(c.Routes) != 2 {
		t.Fatalf("parsed %s r=%v pins=%d links=%d routes=%d", c.PlanetType, c.RadiusKm, len(c.Pins), len(c.Links), len(c.Routes))
	}
	if c.Pins[0].Kind != PinAdvanced || c.Pins[0].Product != "Data Chips" || c.Pins[2].Kind != PinLaunchpad {
		t.Fatalf("pins read as %+v", c.Pins)
	}
	if c.Routes[0].Item != "Microfiber Shielding" || c.Routes[0].Path[0] != 2 {
		t.Fatalf("route read as %+v", c.Routes[0])
	}
	out, err := c.Template()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseTemplate(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Pins) != 3 || back.Links[1].Level != 1 || back.Routes[1].Qty != 3 || back.Pins[1].Product != "Data Chips" {
		t.Fatalf("round trip lost data: %s", out)
	}
	// The game's link cost: base plus the per-km share scaled by
	// (level+1)^modifier (EVE University's 500 km table: level 1 is
	// 280 tf and 183 MW).
	if f := LinkFit(500.45, 1); f.CPU != 280 || f.PG != 183 {
		t.Fatalf("level-1 link at 500 km = %+v, want 280 tf / 183 MW", f)
	}
}

// TestSchematicIDsMatchRecipes: every game schematic id names a
// recipe the planner knows, and every recipe has its id, so colonies
// read from the game and templates written for it agree on products.
func TestSchematicIDsMatchRecipes(t *testing.T) {
	for id, name := range SchematicOutputs {
		if _, ok := Schematics[name]; !ok {
			t.Errorf("schematic %d makes %q, which has no recipe", id, name)
		}
	}
	for name := range Schematics {
		if _, ok := SchematicIDs[name]; !ok {
			t.Errorf("recipe %q has no schematic id", name)
		}
		if _, ok := CommodityTypeIDs[name]; !ok {
			t.Errorf("recipe %q has no type id", name)
		}
	}
}

// TestOptimizeBuildsFittingColonies: a plan from nothing for a P2
// fills the pilot's planets with colonies that fit their command
// centers, imports into the game, makes the product, and is proven
// best for its model.
func TestOptimizeBuildsFittingColonies(t *testing.T) {
	sol, err := Optimize(Request{
		Target: "Coolant", TaxRate: 0.1, SellSurplus: true,
		Characters: []Character{{ID: 1, Name: "Pilot", MaxPlanets: 6, CCLevel: 5}},
		Prices:     map[string]float64{"Coolant": 15000},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sol.Short != "" || sol.PerDay < 100 || !sol.Exact || len(sol.Colonies) == 0 || len(sol.Colonies) > 6 {
		t.Fatalf("plan: %d colonies, %.0f a day, exact %v, short %q", len(sol.Colonies), sol.PerDay, sol.Exact, sol.Short)
	}
	for _, c := range sol.Colonies {
		if !c.Fit.Fits() {
			t.Errorf("%s does not fit: %+v", c.PlanetName, c.Fit)
		}
		if _, err := c.Colony.Template(); err != nil {
			t.Errorf("%s template: %v", c.PlanetName, err)
		}
		if len(c.Colony.Crowded()) > 0 {
			t.Errorf("%s has structures closer than the game allows", c.PlanetName)
		}
	}
}

// TestBuyMissingBuysOnlyWhatCannotBeMade holds the "only what my
// planets cannot make" setting to its word: with only Barren planets
// in reach, Coolant's Electrolytes side (Ionic Solutions comes from
// Gas and Storm) is bought, and the Water side is made even when
// buying Water is cheaper.
func TestBuyMissingBuysOnlyWhatCannotBeMade(t *testing.T) {
	sol, err := Optimize(Request{
		Target: "Coolant", TaxRate: 0.1, BuyMissing: true,
		Characters: []Character{{ID: 1, Name: "Pilot", MaxPlanets: 6, CCLevel: 5}},
		Candidates: []Candidate{{PlanetID: 1, Name: "B1", Type: "Barren", RadiusKm: 4000}, {PlanetID: 2, Name: "B2", Type: "Barren", RadiusKm: 4000}},
		Prices:     map[string]float64{"Coolant": 15000, "Water": 1, "Electrolytes": 400, "Aqueous Liquids": 1, "Ionic Solutions": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sol.PerDay < 1 {
		t.Fatalf("no Coolant made: %q", sol.Short)
	}
	for _, f := range sol.Flows {
		if f.From == -1 && (f.Item == "Water" || f.Item == "Aqueous Liquids") {
			t.Errorf("bought %s, which the Barren planets make", f.Item)
		}
	}
}

// TestFitLeavesOutRemovedStructures holds a changed colony's budget to
// what stands after the change: a structure marked for removal draws
// no CPU or power.
func TestFitLeavesOutRemovedStructures(t *testing.T) {
	c := &Colony{PlanetType: "Barren", RadiusKm: 5000, CCLevel: 1, Pins: []Pin{
		{Kind: PinCommandCenter, Keep: ChangeKeep},
		{Kind: PinLaunchpad, Keep: ChangeKeep},
		{Kind: PinLaunchpad, Keep: ChangeRemove},
	}}
	if f := c.Fit(); f.CPUUsed != PinFits[PinLaunchpad].CPU || !f.Fits() {
		t.Errorf("fit counts the removed launchpad: %+v", f)
	}
}

// TestRicherPlanetsKeepTargetReachable holds a target the planets can
// reach to stay reachable when the per-head yield goes up: a higher
// yield never turns a working plan into "cannot make that much".
func TestRicherPlanetsKeepTargetReachable(t *testing.T) {
	var cands []Candidate
	for _, pt := range PlanetTypes {
		for i := 0; i < 6; i++ {
			cands = append(cands, Candidate{PlanetID: int64(len(cands) + 1), Name: pt, Type: pt, RadiusKm: TypicalRadiusKm[pt]})
		}
	}
	var chars []Character
	for i := 1; i <= 5; i++ {
		chars = append(chars, Character{ID: int64(i), Name: "Pilot", MaxPlanets: 6, CCLevel: 5})
	}
	for _, peak := range []float64{3000, 6000} {
		sol, err := Optimize(Request{
			Target: "Integrity Response Drones", PerDay: 10, Peak: peak, TaxRate: 0.1,
			Characters: chars, Candidates: cands,
		})
		if err != nil {
			t.Fatal(err)
		}
		if sol.PerDay < 10-1e-6 {
			t.Errorf("peak %.0f: %.2f a day, %q", peak, sol.PerDay, sol.Short)
		}
	}
}

// TestBuyMissingBuysWhatOnlyFullPlanetsSupply holds "only what my
// planets cannot make" to the planets a plan can use: with every slot
// held by standing Barren colonies, a Gas planet in reach cannot host
// a colony, so Electrolytes are bought and Coolant is still made.
func TestBuyMissingBuysWhatOnlyFullPlanetsSupply(t *testing.T) {
	cands := []Candidate{{PlanetID: 9, Name: "G1", Type: "Gas", RadiusKm: 20000}}
	for id := int64(1); id <= 2; id++ {
		cands = append(cands, Candidate{
			PlanetID: id, Name: "B", Type: "Barren", RadiusKm: 4000, OwnerID: 1,
			Existing: &Colony{PlanetID: id, PlanetType: "Barren", RadiusKm: 4000, CCLevel: 5, Pins: []Pin{{Kind: PinCommandCenter, Keep: ChangeKeep}}},
		})
	}
	sol, err := Optimize(Request{
		Target: "Coolant", TaxRate: 0.1, BuyMissing: true, Adapt: true,
		Characters: []Character{{ID: 1, Name: "Pilot", MaxPlanets: 2, CCLevel: 5}},
		Candidates: cands,
		Prices:     map[string]float64{"Coolant": 15000, "Water": 1, "Electrolytes": 400, "Aqueous Liquids": 1, "Ionic Solutions": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sol.PerDay < 1 {
		t.Fatalf("no Coolant made: %q", sol.Short)
	}
	bought := false
	for _, f := range sol.Flows {
		if f.From == -1 && f.Item == "Electrolytes" {
			bought = true
		}
	}
	if !bought {
		t.Errorf("Electrolytes not bought; flows %+v", sol.Flows)
	}
}
