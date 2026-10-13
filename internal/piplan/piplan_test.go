package piplan

import (
	"math"
	"strings"
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

// TestTierVolumes pins the built-in volume of every tier to the
// SDE's figures (invTypes.volume, dump of 2026-10-13: every
// commodity of a tier shares one volume). These changed once
// already, when PI volumes were halved; if an SDE import shows
// different ones again, change them here on purpose.
func TestTierVolumes(t *testing.T) {
	want := map[Tier]float64{TierRaw: 0.005, TierP1: 0.19, TierP2: 0.75, TierP3: 3, TierP4: 50}
	counts := map[Tier]int{TierRaw: 15, TierP1: 15, TierP2: 24, TierP3: 21, TierP4: 8}
	for tier, volume := range want {
		names := ByTier(tier)
		if len(names) != counts[tier] {
			t.Errorf("%s has %d commodities, want %d", tier, len(names), counts[tier])
		}
		for _, name := range names {
			if got := AllCommodities[name].Volume; got != volume {
				t.Errorf("%s (%s) volume = %v m3, want %v", name, tier, got, volume)
			}
		}
	}
	for name, volume := range map[string]float64{
		"Aqueous Liquids": 0.005, "Water": 0.19, "Coolant": 0.75, "Robotics": 3, "Broadcast Node": 50,
	} {
		if got := AllCommodities[name].Volume; got != volume {
			t.Errorf("%s volume = %v m3, want %v", name, got, volume)
		}
	}
}

// TestPlanUsesGivenVolumes: a caller's volume for a commodity
// (its SDE figure) replaces the built-in one for that line only;
// a missing or non-positive entry keeps the built-in figure.
func TestPlanUsesGivenVolumes(t *testing.T) {
	plan, err := PlanFor("Coolant", 240, Settings{Hours: 24, Volumes: map[string]float64{"Coolant": 2, "Water": 0}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, ln := range plan.Lines {
		got[ln.Name] = ln.Volume
	}
	if got["Coolant"] != 480 {
		t.Errorf("Coolant volume = %v m3, want 240 x 2 = 480", got["Coolant"])
	}
	if math.Abs(got["Water"]-1920*0.19) > 1e-9 {
		t.Errorf("Water volume = %v m3, want the built-in 1920 x 0.19", got["Water"])
	}
	if math.Abs(got["Electrolytes"]-1920*0.19) > 1e-9 {
		t.Errorf("Electrolytes volume = %v m3, want the built-in 1920 x 0.19", got["Electrolytes"])
	}
}

// TestRawExtractedOnOnePlanet: when two planets of the suggested
// cover both yield a raw the chain needs, its extractors are
// counted on one of them, once. Checked for every product, and
// at least one product must actually have such a shared raw.
func TestRawExtractedOnOnePlanet(t *testing.T) {
	s := Settings{Hours: 24, HeadsPerECU: 10, ECUYield: 12000}
	sawShared := false
	for _, target := range AllTargets() {
		plan, err := PlanFor(target, 10, s, nil)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		cover := MinimalPlanets(target)
		for _, ln := range plan.ByTier[TierRaw] {
			yielding := 0
			for _, pt := range cover {
				for _, raw := range PlanetYields[pt] {
					if raw == ln.Name {
						yielding++
					}
				}
			}
			if yielding > 1 {
				sawShared = true
			}
			colonies := 0
			for _, c := range plan.Colonies {
				if c.Role != "Extractor" {
					continue
				}
				for _, part := range c.Breakdown {
					if strings.HasPrefix(part, ln.Name+": ") && strings.Contains(part, "ECU") {
						colonies++
					}
				}
			}
			if colonies != 1 {
				t.Errorf("%s: %s is extracted on %d colonies (cover %v yields it %d times), want 1",
					target, ln.Name, colonies, cover, yielding)
			}
		}
	}
	if !sawShared {
		t.Fatal("no product has a raw yielded by two planets of its cover: the test checks nothing")
	}
}

// TestExtractorColonyLoad pins one colony's arithmetic: a raw
// needed at 24,000 an hour with 12,000 per ECU is two ECUs of
// ten heads, on one planet.
func TestExtractorColonyLoad(t *testing.T) {
	// 80 Water an hour needs 12,000 Aqueous Liquids an hour: 1920 Water over 24h.
	plan, err := PlanFor("Water", 3840, Settings{Hours: 24, HeadsPerECU: 10, ECUYield: 12000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var extractors []ColonyPlan
	for _, c := range plan.Colonies {
		if c.Role == "Extractor" {
			extractors = append(extractors, c)
		}
	}
	if len(extractors) != 1 {
		t.Fatalf("%d extractor colonies for a one-raw chain, want 1", len(extractors))
	}
	// 3840 Water = 192 cycles x 3000 = 576,000 raw over 24h = 24,000/hr -> 2 ECUs.
	ecu, head := Facilities["Extractor Control Unit"], Facilities["Extractor Head"]
	basic, launch := Facilities["Basic Industry Facility"], Facilities["Launchpad"]
	// 3840/24 = 160 Water/hr; a Basic makes 40/hr -> 4 factories.
	wantCPU := 2*(ecu.CPU+10*head.CPU) + 4*basic.CPU + launch.CPU
	if extractors[0].CPUUsed != wantCPU {
		t.Fatalf("colony CPU = %d, want %d (2 ECUs of 10 heads, 4 Basic, 1 Launchpad)", extractors[0].CPUUsed, wantCPU)
	}
}

// TestProfitIsAfterPOCOTax: the profit figures pay the taxes
// shown beside them. Worked example, 240 Coolant at 10%:
//
//	export  240 x 7,200 x 10%            = 172,800
//	import  3,840 P1 x 400 x 10% x 0.5   =  76,800
//
// The whole chain and the direct step import the same P1 here,
// so both pay 249,600 in tax.
func TestProfitIsAfterPOCOTax(t *testing.T) {
	prices := map[string]float64{
		"Coolant": 10000, "Water": 300, "Electrolytes": 300,
		"Aqueous Liquids": 1, "Ionic Solutions": 1,
	}
	plan, err := PlanFor("Coolant", 240, Settings{Hours: 24, TaxRate: 0.10}, prices)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExportTax != 172800 || plan.ImportTax != 76800 || plan.StepImportTax != 76800 {
		t.Fatalf("taxes: export %v import %v step import %v", plan.ExportTax, plan.ImportTax, plan.StepImportTax)
	}
	// Revenue 2,400,000. Raw: 576,000 units at 1 ISK. Step: 3,840 P1 at 300.
	if want := 2400000.0 - 576000 - 172800 - 76800; plan.ProfitWhole != want {
		t.Errorf("whole-chain profit = %v, want %v (after both taxes)", plan.ProfitWhole, want)
	}
	if want := 2400000.0 - 1152000 - 172800 - 76800; plan.ProfitStep != want {
		t.Errorf("step profit = %v, want %v (after both taxes)", plan.ProfitStep, want)
	}
	if want := plan.ProfitWhole / 2400000 * 100; math.Abs(plan.MarginWhole-want) > 1e-9 {
		t.Errorf("whole-chain margin = %v, want %v", plan.MarginWhole, want)
	}

	// With no tax the profit is plain revenue less inputs.
	untaxed, err := PlanFor("Coolant", 240, Settings{Hours: 24}, prices)
	if err != nil {
		t.Fatal(err)
	}
	if untaxed.ProfitWhole != 2400000-576000 || untaxed.ProfitStep != 2400000-1152000 {
		t.Errorf("untaxed profit: whole %v step %v", untaxed.ProfitWhole, untaxed.ProfitStep)
	}
}
