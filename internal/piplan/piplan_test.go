package piplan

import (
	"math"
	"testing"
)

func TestP1Schematic(t *testing.T) {
	s, ok := Schematics["Bacteria"]
	if !ok {
		t.Fatal("Bacteria schematic missing")
	}
	if s.Inputs["Microorganisms"] != 3000 || s.OutputQty != 20 || s.CycleSec != 1800 {
		t.Fatalf("Bacteria schematic = %+v", s)
	}
	if TierOf("Bacteria") != TierP1 || TierOf("Microorganisms") != TierRaw {
		t.Fatal("tier mapping wrong")
	}
}

func TestChainBroadcastNode(t *testing.T) {
	raws := RequiredRaws("Broadcast Node")
	// Broadcast Node needs many raws; at minimum it must include
	// the raws behind Neocoms/Data Chips/High-Tech Transmitters.
	if len(raws) < 6 {
		t.Fatalf("Broadcast Node raws = %v, want >=6", raws)
	}
	nodes := ChainNodes("Broadcast Node")
	if len(nodes) < 15 {
		t.Fatalf("Broadcast Node chain nodes = %d, want >=15", len(nodes))
	}
	// Every P4 needs exactly 3 P3 inputs at 6 each.
	plan, err := PlanFor("Broadcast Node", 1, Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 1 unit needs 6 of each P3; each P3 needs 10 of each P2 per 3 units.
	// Neocoms needs 6 -> 2 cycles -> 20 Biocells + 20 Silicate Glass.
	found := map[string]float64{}
	for _, ln := range plan.Lines {
		found[ln.Name] = ln.Needed
	}
	if found["Neocoms"] != 6 || found["Data Chips"] != 6 {
		t.Fatalf("P3 needs = %v", found)
	}
	if math.Abs(found["Biocells"]-20) > 0.01 {
		t.Fatalf("Biocells need = %v, want 20", found["Biocells"])
	}
}

func TestCoverageAndMinimalPlanets(t *testing.T) {
	// Coolant needs Water + Electrolytes raws: Aqueous Liquids + Ionic Solutions.
	// Gas alone yields both, so single-planet must be Gas (or Storm? Storm has
	// Ionic + Aqueous too — either is fine, just require completeness).
	cov := CoverageFor("Coolant", []string{"Gas"})
	if !cov.Complete {
		t.Fatalf("Gas should cover Coolant, got %+v", cov)
	}
	cov2 := CoverageFor("Coolant", []string{"Lava"})
	if cov2.Complete {
		t.Fatal("Lava must not cover Coolant")
	}
	if len(cov2.Missing) == 0 {
		t.Fatal("Lava coverage should report missing raws")
	}
	min := MinimalPlanets("Broadcast Node")
	cov3 := CoverageFor("Broadcast Node", min)
	if !cov3.Complete {
		t.Fatalf("minimal planets %v do not cover Broadcast Node: %+v", min, cov3)
	}
}

func TestPlanEconomicsAndFactories(t *testing.T) {
	prices := map[string]float64{
		"Broadcast Node":  10000000,
		"Aqueous Liquids": 10, "Ionic Solutions": 10,
	}
	plan, err := PlanFor("Coolant", 240, Settings{Hours: 24}, prices)
	if err != nil {
		t.Fatal(err)
	}
	// Coolant: 240 units = 48 cycles (5/cycle) -> 1920 Water + 1920 Electrolytes.
	// Water: 1920 units = 96 cycles -> 288000 Aqueous Liquids.
	found := map[string]float64{}
	for _, ln := range plan.Lines {
		found[ln.Name] = ln.Needed
	}
	if found["Water"] != 1920 || found["Electrolytes"] != 1920 {
		t.Fatalf("P1 needs = %v", found)
	}
	if found["Aqueous Liquids"] != 288000 {
		t.Fatalf("raw need = %v", found["Aqueous Liquids"])
	}
	// Factories: Coolant output 240/day = 10/hr; one Advanced makes 5/hr -> 2.
	for _, ln := range plan.Lines {
		if ln.Name == "Coolant" && ln.Factories != 2 {
			t.Fatalf("Coolant factories = %d, want 2", ln.Factories)
		}
	}
	if len(plan.Colonies) == 0 {
		t.Fatal("plan should suggest colonies")
	}
}

func TestUnknownAndRawRejected(t *testing.T) {
	if _, err := PlanFor("Not A Thing", 1, Settings{}, nil); err == nil {
		t.Fatal("unknown commodity should error")
	}
	if _, err := PlanFor("Aqueous Liquids", 1, Settings{}, nil); err == nil {
		t.Fatal("raw target should error")
	}
}
