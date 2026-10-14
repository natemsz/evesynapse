// Package piplan is the Planetary Interaction planner engine.
//
// It models the full PI chain Raw (R0) -> P1 -> P2 -> P3 -> P4
// with the canonical EVE schematics, planet yields, facility
// power/CPU and POCO tax values. The engine is pure: callers
// supply market prices keyed by commodity name and it returns
// requirements, factory counts, fitting and economics. It does
// no I/O.
package piplan

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Tier identifies a PI production tier.
type Tier string

const (
	TierRaw Tier = "R0"
	TierP1  Tier = "P1"
	TierP2  Tier = "P2"
	TierP3  Tier = "P3"
	TierP4  Tier = "P4"
)

// Commodity is one PI material.
type Commodity struct {
	Name     string
	Tier     Tier
	Volume   float64 // m3 per unit
	TaxValue float64 // CCP taxable value (ISK) for POCO fees
}

// FacilityKind is the processor family a schematic runs in.
type FacilityKind string

const (
	FacilityBasic    FacilityKind = "Basic"
	FacilityAdvanced FacilityKind = "Advanced"
	FacilityHighTech FacilityKind = "High-Tech"
)

// Schematic is one factory recipe: Inputs consumed per cycle,
// OutputQty produced per cycle.
type Schematic struct {
	Output    string
	Inputs    map[string]int64 // commodity name -> units per cycle
	OutputQty int64
	CycleSec  int64
	Facility  FacilityKind
}

// FacilityStats is CPU/Power for one planetary structure.
type FacilityStats struct {
	Name string
	CPU  int64 // tf
	PG   int64 // MW
	Cost int64 // ISK to build
}

// CommandCenter is one CC upgrade level.
type CommandCenter struct {
	Level int
	CPU   int64
	PG    int64
	Cost  int64
}

// AllCommodities is every PI commodity by name.
var AllCommodities map[string]Commodity

// Schematics by output commodity name.
var Schematics map[string]Schematic

// PlanetTypes in display order.
var PlanetTypes = []string{"Barren", "Gas", "Ice", "Lava", "Oceanic", "Plasma", "Storm", "Temperate"}

// PlanetYields maps planet type -> raw resources found there.
var PlanetYields map[string][]string

// RawSources maps raw resource -> planet types that yield it.
var RawSources map[string][]string

// Facilities by kind/name.
var Facilities = map[string]FacilityStats{
	"Extractor Control Unit":      {Name: "Extractor Control Unit", CPU: 400, PG: 2600, Cost: 45000},
	"Extractor Head":              {Name: "Extractor Head", CPU: 110, PG: 550, Cost: 0},
	"Basic Industry Facility":     {Name: "Basic Industry Facility", CPU: 200, PG: 800, Cost: 75000},
	"Advanced Industry Facility":  {Name: "Advanced Industry Facility", CPU: 500, PG: 700, Cost: 250000},
	"High-Tech Industry Facility": {Name: "High-Tech Industry Facility", CPU: 1100, PG: 400, Cost: 525000},
	"Storage Facility":            {Name: "Storage Facility", CPU: 500, PG: 700, Cost: 250000},
	"Launchpad":                   {Name: "Launchpad", CPU: 3600, PG: 700, Cost: 900000},
}

// CommandCenters by level 0..5.
var CommandCenters = []CommandCenter{
	{Level: 0, CPU: 1675, PG: 6000, Cost: 0},
	{Level: 1, CPU: 7057, PG: 9000, Cost: 580000},
	{Level: 2, CPU: 12136, PG: 12000, Cost: 930000},
	{Level: 3, CPU: 17215, PG: 15000, Cost: 1200000},
	{Level: 4, CPU: 21315, PG: 17000, Cost: 1500000},
	{Level: 5, CPU: 25415, PG: 19000, Cost: 2100000},
}

func init() {
	AllCommodities = map[string]Commodity{}
	add := func(name string, tier Tier, vol, tax float64) {
		AllCommodities[name] = Commodity{Name: name, Tier: tier, Volume: vol, TaxValue: tax}
	}
	// R0 — volume 0.01 m3, taxable 5 ISK.
	for _, n := range []string{
		"Aqueous Liquids", "Autotrophs", "Base Metals", "Carbon Compounds",
		"Complex Organisms", "Felsic Magma", "Heavy Metals", "Ionic Solutions",
		"Microorganisms", "Noble Gas", "Noble Metals", "Non-CS Crystals",
		"Planktic Colonies", "Reactive Gas", "Suspended Plasma",
	} {
		add(n, TierRaw, 0.01, 5)
	}
	// P1 — volume 0.38 m3, taxable 400 ISK.
	for _, n := range []string{
		"Bacteria", "Biofuels", "Biomass", "Chiral Structures", "Electrolytes",
		"Industrial Fibers", "Oxidizing Compound", "Oxygen", "Plasmoids",
		"Precious Metals", "Proteins", "Reactive Metals", "Silicon",
		"Toxic Metals", "Water",
	} {
		add(n, TierP1, 0.38, 400)
	}
	// P2 — volume 0.75 m3, taxable 7200 ISK (checked: 40+40 -> 5).
	for _, n := range []string{
		"Biocells", "Construction Blocks", "Consumer Electronics", "Coolant",
		"Enriched Uranium", "Fertilizer", "Genetically Enhanced Livestock",
		"Livestock", "Mechanical Parts", "Microfiber Shielding",
		"Miniature Electronics", "Nanites", "Oxides", "Polyaramids",
		"Polytextiles", "Rocket Fuel", "Silicate Glass", "Superconductors",
		"Supertensile Plastics", "Synthetic Oil", "Test Cultures",
		"Transmitter", "Viral Agent", "Water-Cooled CPU",
	} {
		add(n, TierP2, 0.75, 7200)
	}
	// P3 — volume 3 m3, taxable 60000 ISK.
	for _, n := range []string{
		"Biotech Research Reports", "Camera Drones", "Condensates",
		"Cryoprotectant Solution", "Data Chips", "Gel-Matrix Biopaste",
		"Guidance Systems", "Hazmat Detection Systems", "Hermetic Membranes",
		"High-Tech Transmitters", "Industrial Explosives", "Neocoms",
		"Nuclear Reactors", "Planetary Vehicles", "Robotics", "Smartfab Units",
		"Supercomputers", "Synthetic Synapses", "Transcranial Microcontrollers",
		"Ukomi Superconductors", "Vaccines",
	} {
		add(n, TierP3, 3, 60000)
	}
	// P4 — volume 50 m3 (Broadcast Node family 100? canonical 50, taxable 1.2M).
	// Sterile Conduits / Wetware etc share 100? EVE uses 100 for some P4;
	// keep 100 for accuracy where known, default 50 is corrected below.
	for _, n := range []string{
		"Broadcast Node", "Integrity Response Drones", "Nano-Factory",
		"Organic Mortar Applicators", "Recursive Computing Module",
		"Self-Harmonizing Power Core", "Sterile Conduits", "Wetware Mainframe",
	} {
		add(n, TierP4, 100, 1200000)
	}

	Schematics = map[string]Schematic{}
	p1 := func(out, raw string) {
		Schematics[out] = Schematic{Output: out, Inputs: map[string]int64{raw: 3000}, OutputQty: 20, CycleSec: 1800, Facility: FacilityBasic}
	}
	p1("Bacteria", "Microorganisms")
	p1("Biofuels", "Carbon Compounds")
	p1("Biomass", "Planktic Colonies")
	p1("Chiral Structures", "Non-CS Crystals")
	p1("Electrolytes", "Ionic Solutions")
	p1("Industrial Fibers", "Autotrophs")
	p1("Oxidizing Compound", "Reactive Gas")
	p1("Oxygen", "Noble Gas")
	p1("Plasmoids", "Suspended Plasma")
	p1("Precious Metals", "Noble Metals")
	p1("Proteins", "Complex Organisms")
	p1("Reactive Metals", "Base Metals")
	p1("Silicon", "Felsic Magma")
	p1("Toxic Metals", "Heavy Metals")
	p1("Water", "Aqueous Liquids")

	p2 := func(out, a, b string) {
		Schematics[out] = Schematic{Output: out, Inputs: map[string]int64{a: 40, b: 40}, OutputQty: 5, CycleSec: 3600, Facility: FacilityAdvanced}
	}
	p2("Biocells", "Precious Metals", "Biofuels")
	p2("Construction Blocks", "Toxic Metals", "Reactive Metals")
	p2("Consumer Electronics", "Chiral Structures", "Toxic Metals")
	p2("Coolant", "Water", "Electrolytes")
	p2("Enriched Uranium", "Toxic Metals", "Precious Metals")
	p2("Fertilizer", "Proteins", "Bacteria")
	p2("Genetically Enhanced Livestock", "Biomass", "Proteins")
	p2("Livestock", "Biofuels", "Proteins")
	p2("Mechanical Parts", "Precious Metals", "Reactive Metals")
	p2("Microfiber Shielding", "Silicon", "Industrial Fibers")
	p2("Miniature Electronics", "Silicon", "Chiral Structures")
	p2("Nanites", "Reactive Metals", "Bacteria")
	p2("Oxides", "Oxygen", "Oxidizing Compound")
	p2("Polyaramids", "Industrial Fibers", "Oxidizing Compound")
	p2("Polytextiles", "Industrial Fibers", "Biofuels")
	p2("Rocket Fuel", "Electrolytes", "Plasmoids")
	p2("Silicate Glass", "Silicon", "Oxidizing Compound")
	p2("Superconductors", "Water", "Plasmoids")
	p2("Supertensile Plastics", "Biomass", "Oxygen")
	p2("Synthetic Oil", "Oxygen", "Electrolytes")
	p2("Test Cultures", "Water", "Bacteria")
	p2("Transmitter", "Chiral Structures", "Plasmoids")
	p2("Viral Agent", "Biomass", "Bacteria")
	p2("Water-Cooled CPU", "Water", "Reactive Metals")

	p3 := func(out string, ins map[string]int64) {
		Schematics[out] = Schematic{Output: out, Inputs: ins, OutputQty: 3, CycleSec: 3600, Facility: FacilityAdvanced}
	}
	p3("Biotech Research Reports", map[string]int64{"Nanites": 10, "Livestock": 10, "Construction Blocks": 10})
	p3("Camera Drones", map[string]int64{"Silicate Glass": 10, "Rocket Fuel": 10})
	p3("Condensates", map[string]int64{"Oxides": 10, "Coolant": 10})
	p3("Cryoprotectant Solution", map[string]int64{"Test Cultures": 10, "Synthetic Oil": 10, "Fertilizer": 10})
	p3("Data Chips", map[string]int64{"Supertensile Plastics": 10, "Microfiber Shielding": 10})
	p3("Gel-Matrix Biopaste", map[string]int64{"Oxides": 10, "Biocells": 10, "Superconductors": 10})
	p3("Guidance Systems", map[string]int64{"Water-Cooled CPU": 10, "Transmitter": 10})
	p3("Hazmat Detection Systems", map[string]int64{"Polytextiles": 10, "Viral Agent": 10, "Transmitter": 10})
	p3("Hermetic Membranes", map[string]int64{"Polyaramids": 10, "Genetically Enhanced Livestock": 10})
	p3("High-Tech Transmitters", map[string]int64{"Polyaramids": 10, "Transmitter": 10})
	p3("Industrial Explosives", map[string]int64{"Fertilizer": 10, "Polytextiles": 10})
	p3("Neocoms", map[string]int64{"Biocells": 10, "Silicate Glass": 10})
	p3("Nuclear Reactors", map[string]int64{"Microfiber Shielding": 10, "Enriched Uranium": 10})
	p3("Planetary Vehicles", map[string]int64{"Supertensile Plastics": 10, "Mechanical Parts": 10, "Miniature Electronics": 10})
	p3("Robotics", map[string]int64{"Mechanical Parts": 10, "Consumer Electronics": 10})
	p3("Smartfab Units", map[string]int64{"Construction Blocks": 10, "Miniature Electronics": 10})
	p3("Supercomputers", map[string]int64{"Water-Cooled CPU": 10, "Coolant": 10, "Consumer Electronics": 10})
	p3("Synthetic Synapses", map[string]int64{"Supertensile Plastics": 10, "Test Cultures": 10})
	p3("Transcranial Microcontrollers", map[string]int64{"Biocells": 10, "Nanites": 10})
	p3("Ukomi Superconductors", map[string]int64{"Synthetic Oil": 10, "Superconductors": 10})
	p3("Vaccines", map[string]int64{"Livestock": 10, "Viral Agent": 10})

	p4 := func(out string, ins map[string]int64) {
		Schematics[out] = Schematic{Output: out, Inputs: ins, OutputQty: 1, CycleSec: 3600, Facility: FacilityHighTech}
	}
	p4("Broadcast Node", map[string]int64{"Neocoms": 6, "Data Chips": 6, "High-Tech Transmitters": 6})
	p4("Integrity Response Drones", map[string]int64{"Gel-Matrix Biopaste": 6, "Hazmat Detection Systems": 6, "Planetary Vehicles": 6})
	p4("Nano-Factory", map[string]int64{"Industrial Explosives": 6, "Ukomi Superconductors": 6, "Reactive Metals": 40})
	p4("Organic Mortar Applicators", map[string]int64{"Condensates": 6, "Robotics": 6, "Bacteria": 40})
	p4("Recursive Computing Module", map[string]int64{"Synthetic Synapses": 6, "Guidance Systems": 6, "Transcranial Microcontrollers": 6})
	p4("Self-Harmonizing Power Core", map[string]int64{"Camera Drones": 6, "Nuclear Reactors": 6, "Hermetic Membranes": 6})
	p4("Sterile Conduits", map[string]int64{"Smartfab Units": 6, "Vaccines": 6, "Water": 40})
	p4("Wetware Mainframe", map[string]int64{"Supercomputers": 6, "Biotech Research Reports": 6, "Cryoprotectant Solution": 6})

	PlanetYields = map[string][]string{
		"Barren":    {"Microorganisms", "Carbon Compounds", "Noble Metals", "Base Metals", "Aqueous Liquids"},
		"Gas":       {"Ionic Solutions", "Reactive Gas", "Noble Gas", "Base Metals", "Aqueous Liquids"},
		"Ice":       {"Microorganisms", "Planktic Colonies", "Noble Gas", "Heavy Metals", "Aqueous Liquids"},
		"Lava":      {"Non-CS Crystals", "Suspended Plasma", "Base Metals", "Felsic Magma", "Heavy Metals"},
		"Oceanic":   {"Microorganisms", "Carbon Compounds", "Planktic Colonies", "Complex Organisms", "Aqueous Liquids"},
		"Plasma":    {"Non-CS Crystals", "Suspended Plasma", "Noble Metals", "Base Metals", "Heavy Metals"},
		"Storm":     {"Ionic Solutions", "Noble Gas", "Suspended Plasma", "Base Metals", "Aqueous Liquids"},
		"Temperate": {"Microorganisms", "Carbon Compounds", "Autotrophs", "Complex Organisms", "Aqueous Liquids"},
	}
	RawSources = map[string][]string{}
	for _, pt := range PlanetTypes {
		for _, raw := range PlanetYields[pt] {
			RawSources[raw] = append(RawSources[raw], pt)
		}
	}
	for k := range RawSources {
		sort.Strings(RawSources[k])
	}
}

// ByTier returns commodity names in a tier, sorted.
func ByTier(t Tier) []string {
	var out []string
	for _, c := range AllCommodities {
		if c.Tier == t {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// TierOf returns the tier of a commodity ("" when unknown).
func TierOf(name string) Tier {
	if c, ok := AllCommodities[name]; ok {
		return c.Tier
	}
	return ""
}

// InputsOf returns the direct schematic inputs for a commodity
// (nil for raw resources).
func InputsOf(name string) map[string]int64 {
	if s, ok := Schematics[name]; ok {
		return s.Inputs
	}
	return nil
}

// RawForP1 maps a P1 commodity to its single raw input.
func RawForP1(p1 string) (string, bool) {
	s, ok := Schematics[p1]
	if !ok {
		return "", false
	}
	for k := range s.Inputs {
		return k, true
	}
	return "", false
}

// RequiredRaws collects every raw resource needed (transitively)
// to make target, deduplicated and sorted.
func RequiredRaws(target string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if TierOf(name) == TierRaw {
			seen[name] = true
			return
		}
		for in := range InputsOf(name) {
			walk(in)
		}
	}
	walk(target)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ChainNodes returns the full dependency closure of target
// including target itself, sorted by tier then name. Planets
// are not included.
func ChainNodes(target string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		for in := range InputsOf(name) {
			walk(in)
		}
	}
	walk(target)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := TierOf(out[i]), TierOf(out[j])
		if ti != tj {
			return tierRank(ti) < tierRank(tj)
		}
		return out[i] < out[j]
	})
	return out
}

func tierRank(t Tier) int {
	switch t {
	case TierRaw:
		return 0
	case TierP1:
		return 1
	case TierP2:
		return 2
	case TierP3:
		return 3
	case TierP4:
		return 4
	}
	return 5
}

// Coverage reports whether the selected planet types can supply
// every raw needed for target.
type Coverage struct {
	Required []string
	Covered  []string
	Missing  []string
	Complete bool
}

// CoverageFor checks required raws against planet types.
//
// static/pi.js mirrors this check live in the browser (ticking
// planets): change both.
func CoverageFor(target string, planets []string) Coverage {
	req := RequiredRaws(target)
	avail := map[string]bool{}
	for _, p := range planets {
		for _, raw := range PlanetYields[titlePlanet(p)] {
			avail[raw] = true
		}
	}
	cov := Coverage{Required: req}
	for _, r := range req {
		if avail[r] {
			cov.Covered = append(cov.Covered, r)
		} else {
			cov.Missing = append(cov.Missing, r)
		}
	}
	cov.Complete = len(cov.Missing) == 0 && len(req) > 0
	return cov
}

// titlePlanet normalises a planet type string ("barren" -> "Barren").
func titlePlanet(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// SinglePlanet reports whether one planet type alone supplies
// every raw for target.
func SinglePlanet(target string) (string, bool) {
	req := RequiredRaws(target)
	for _, pt := range PlanetTypes {
		yields := map[string]bool{}
		for _, r := range PlanetYields[pt] {
			yields[r] = true
		}
		ok := true
		for _, r := range req {
			if !yields[r] {
				ok = false
				break
			}
		}
		if ok {
			return pt, true
		}
	}
	return "", false
}

// MinimalPlanets finds a small set of planet types covering all
// raws for target (greedy set cover; PI needs at most 5).
func MinimalPlanets(target string) []string {
	req := RequiredRaws(target)
	need := map[string]bool{}
	for _, r := range req {
		need[r] = true
	}
	var chosen []string
	covered := map[string]bool{}
	for len(covered) < len(need) {
		best, bestGain := "", 0
		for _, pt := range PlanetTypes {
			gain := 0
			for _, r := range PlanetYields[pt] {
				if need[r] && !covered[r] {
					gain++
				}
			}
			if gain > bestGain {
				best, bestGain = pt, gain
			}
		}
		if best == "" || bestGain == 0 {
			break
		}
		chosen = append(chosen, best)
		for _, r := range PlanetYields[best] {
			if need[r] {
				covered[r] = true
			}
		}
	}
	sort.Strings(chosen)
	return chosen
}

// --- Planning engine ---

// Settings tune the simulator.
type Settings struct {
	CCLevel     int     // 0..5
	HeadsPerECU int     // extractor heads per ECU (1..10)
	ECUYield    float64 // R0 units per hour per ECU (all heads combined)
	TaxRate     float64 // POCO tax rate as fraction (0.10 = 10%)
	// Per-tier import/export multipliers on the taxable value.
	// Defaults follow EVE: import 0.5x, export 1.0x of taxRate.
	Hours float64 // plan window for factory sizing (default 24)
}

func (s Settings) normalized() Settings {
	if s.CCLevel < 0 {
		s.CCLevel = 0
	}
	if s.CCLevel > 5 {
		s.CCLevel = 5
	}
	if s.HeadsPerECU < 1 {
		s.HeadsPerECU = 10
	}
	if s.HeadsPerECU > 10 {
		s.HeadsPerECU = 10
	}
	if s.ECUYield <= 0 {
		s.ECUYield = 12000 // ~2 basic factories fed per ECU
	}
	if s.Hours <= 0 {
		s.Hours = 24
	}
	return s
}

// Line is one commodity's requirement in a plan.
type Line struct {
	Name       string
	Tier       Tier
	Needed     float64 // total units needed for the batch
	PerHour    float64 // units per hour to sustain the rate
	Cycles     float64 // factory cycles needed (0 for raw)
	Facility   FacilityKind
	Factories  int     // parallel factories to finish in Settings.Hours
	Volume     float64 // total m3
	TaxExport  float64 // ISK to export this line's Needed once
	Price      float64 // unit market price (0 = unknown)
	PriceKnown bool
	LineValue  float64 // Needed * Price when known
}

// ColonyPlan is one suggested planet in the layout.
type ColonyPlan struct {
	Role      string // "Extractor" | "Factory"
	Planet    string // planet type ("" for factory-any)
	Title     string
	Detail    string
	CPUUsed   int64
	PGUsed    int64
	CPUCap    int64
	PGCap     int64
	Fits      bool
	Breakdown []string
}

// Plan is the full simulator result for one target batch.
type Plan struct {
	Target        string
	Amount        int64  // units of Target requested
	Lines         []Line // every tier, target first then by tier desc
	ByTier        map[Tier][]Line
	Colonies      []ColonyPlan
	Revenue       float64
	RevenueKnown  bool
	RawCost       float64 // cost to buy all R0 at market (whole-chain input cost)
	RawCostKnown  bool
	StepCost      float64 // cost to buy direct inputs of target
	StepCostKnown bool
	ProfitWhole   float64
	MarginWhole   float64 // profit / revenue
	ProfitStep    float64
	MarginStep    float64
	ExportTax     float64 // total export tax on final product
	ImportTax     float64 // total import tax if intermediates imported once
}

// PlanFor computes requirements for amount units of target.
// prices is commodity name -> unit price (0/missing = unknown).
func PlanFor(target string, amount int64, s Settings, prices map[string]float64) (*Plan, error) {
	if _, ok := AllCommodities[target]; !ok {
		return nil, fmt.Errorf("unknown PI commodity %q", target)
	}
	if TierOf(target) == TierRaw {
		return nil, fmt.Errorf("%s is a raw resource — pick a P1–P4 product", target)
	}
	if amount < 1 {
		amount = 1
	}
	s = s.normalized()
	cc := CommandCenters[s.CCLevel]

	// Recursive need: cycles drive inputs. Work in float so
	// factory sizing can use exact rates, then round display.
	need := map[string]float64{target: float64(amount)}
	cycles := map[string]float64{}
	// Process tiers descending so children accumulate fully.
	order := ChainNodes(target)
	// ChainNodes is ascending; iterate descending, target first.
	sort.Slice(order, func(i, j int) bool {
		ri, rj := tierRank(TierOf(order[i])), tierRank(TierOf(order[j]))
		if ri != rj {
			return ri > rj
		}
		return order[i] < order[j]
	})
	for _, name := range order {
		qty := need[name]
		if qty <= 0 {
			continue
		}
		sch, ok := Schematics[name]
		if !ok {
			continue // raw leaf
		}
		c := qty / float64(sch.OutputQty)
		cycles[name] = c
		for in, per := range sch.Inputs {
			need[in] += c * float64(per)
		}
	}

	plan := &Plan{Target: target, Amount: amount, ByTier: map[Tier][]Line{}}
	var lines []Line
	for _, name := range order {
		c := AllCommodities[name]
		ln := Line{Name: name, Tier: c.Tier, Needed: need[name]}
		ln.PerHour = ln.Needed / s.Hours
		ln.Volume = ln.Needed * c.Volume
		ln.TaxExport = ln.Needed * c.TaxValue * s.TaxRate
		if p, ok := prices[name]; ok && p > 0 {
			ln.Price, ln.PriceKnown = p, true
			ln.LineValue = ln.Needed * p
		}
		if sch, ok := Schematics[name]; ok {
			ln.Cycles = cycles[name]
			ln.Facility = sch.Facility
			cyclesPerHour := 3600.0 / float64(sch.CycleSec)
			outPerFactoryHour := cyclesPerHour * float64(sch.OutputQty)
			if outPerFactoryHour > 0 {
				ln.Factories = int(math.Ceil(ln.PerHour / outPerFactoryHour))
				if ln.Factories < 1 && ln.Needed > 0 {
					ln.Factories = 1
				}
			}
		}
		lines = append(lines, ln)
		plan.ByTier[c.Tier] = append(plan.ByTier[c.Tier], ln)
	}
	// Lines: target first, then tier descending, name ascending.
	plan.Lines = lines

	// Economics.
	if p, ok := prices[target]; ok && p > 0 {
		plan.Revenue, plan.RevenueKnown = float64(amount)*p, true
	}
	rawOK := true
	for _, ln := range plan.ByTier[TierRaw] {
		if !ln.PriceKnown {
			rawOK = false
			break
		}
		plan.RawCost += ln.LineValue
	}
	plan.RawCostKnown = rawOK && len(plan.ByTier[TierRaw]) > 0
	// Step cost: direct inputs of target.
	if sch, ok := Schematics[target]; ok {
		stepOK := true
		c := cycles[target]
		for in, per := range sch.Inputs {
			p, hit := prices[in]
			if !hit || p <= 0 {
				stepOK = false
				break
			}
			plan.StepCost += c * float64(per) * p
		}
		plan.StepCostKnown = stepOK
	}
	if plan.RevenueKnown && plan.RawCostKnown {
		plan.ProfitWhole = plan.Revenue - plan.RawCost
		if plan.Revenue > 0 {
			plan.MarginWhole = plan.ProfitWhole / plan.Revenue * 100
		}
	}
	if plan.RevenueKnown && plan.StepCostKnown {
		plan.ProfitStep = plan.Revenue - plan.StepCost
		if plan.Revenue > 0 {
			plan.MarginStep = plan.ProfitStep / plan.Revenue * 100
		}
	}
	if c, ok := AllCommodities[target]; ok {
		plan.ExportTax = float64(amount) * c.TaxValue * s.TaxRate
	}
	// Import tax: each intermediate imported once at 0.5x.
	for tier, ls := range plan.ByTier {
		if tier == TierRaw || tier == TierOf(target) {
			continue
		}
		for _, ln := range ls {
			if comm, ok := AllCommodities[ln.Name]; ok {
				plan.ImportTax += ln.Needed * comm.TaxValue * s.TaxRate * 0.5
			}
		}
	}

	plan.Colonies = suggestColonies(target, plan, s, cc)
	return plan, nil
}

func suggestColonies(target string, plan *Plan, s Settings, cc CommandCenter) []ColonyPlan {
	// Extractor colonies: one per planet type in the minimal cover,
	// each extracting its raws and refining them to P1 on-planet.
	minimal := MinimalPlanets(target)
	rawLine := map[string]Line{}
	for _, ln := range plan.ByTier[TierRaw] {
		rawLine[ln.Name] = ln
	}
	p1Line := map[string]Line{}
	for _, ln := range plan.ByTier[TierP1] {
		p1Line[ln.Name] = ln
	}
	// Map P1 -> raw.
	p1Raw := map[string]string{}
	for p1 := range p1Line {
		if r, ok := RawForP1(p1); ok {
			p1Raw[p1] = r
		}
	}

	var out []ColonyPlan
	ecu := Facilities["Extractor Control Unit"]
	head := Facilities["Extractor Head"]
	basic := Facilities["Basic Industry Facility"]
	adv := Facilities["Advanced Industry Facility"]
	high := Facilities["High-Tech Industry Facility"]
	launch := Facilities["Launchpad"]

	for _, pt := range minimal {
		var cpu, pg int64
		var parts []string
		// ECUs needed for raws yielded here that the chain needs.
		ecus := 0
		for _, raw := range PlanetYields[pt] {
			ln, needed := rawLine[raw]
			if !needed {
				continue
			}
			n := int(math.Ceil(ln.PerHour / s.ECUYield))
			if n < 1 {
				n = 1
			}
			ecus += n
			parts = append(parts, fmt.Sprintf("%s: %d ECU × %d heads (%.0f/hr needed)", raw, n, s.HeadsPerECU, ln.PerHour))
		}
		cpu += int64(ecus) * (ecu.CPU + int64(s.HeadsPerECU)*head.CPU)
		pg += int64(ecus) * (ecu.PG + int64(s.HeadsPerECU)*head.PG)
		// Basic factories for P1s whose raw lives here (first cover wins).
		basics := 0
		for p1, raw := range p1Raw {
			// Assign P1 to the first minimal planet yielding its raw.
			assigned := ""
			for _, mp := range minimal {
				for _, y := range PlanetYields[mp] {
					if y == raw {
						assigned = mp
						break
					}
				}
				if assigned != "" {
					break
				}
			}
			if assigned != pt {
				continue
			}
			if ln, ok := p1Line[p1]; ok && ln.Factories > 0 {
				basics += ln.Factories
				parts = append(parts, fmt.Sprintf("%s: %d× Basic", p1, ln.Factories))
			}
		}
		cpu += int64(basics) * basic.CPU
		pg += int64(basics) * basic.PG
		cpu += launch.CPU
		pg += launch.PG
		parts = append(parts, "1× Launchpad")
		out = append(out, ColonyPlan{
			Role: "Extractor", Planet: pt,
			Title:   pt + " extractor",
			Detail:  "Extract and refine to P1 on-planet, export P1 to the factory planet.",
			CPUUsed: cpu, PGUsed: pg, CPUCap: cc.CPU, PGCap: cc.PG,
			Fits: cpu <= cc.CPU && pg <= cc.PG, Breakdown: parts,
		})
	}

	// Factory colony: all P2+ factories. P4 needs Barren/Temperate.
	factPlanet := "Barren or Temperate"
	if TierOf(target) != TierP4 {
		factPlanet = "Any (Barren/Temperate ready for P4 later)"
	}
	var fcpu, fpg int64
	var fparts []string
	advCount, highCount := 0, 0
	for _, ln := range plan.Lines {
		switch ln.Facility {
		case FacilityAdvanced:
			advCount += ln.Factories
		case FacilityHighTech:
			highCount += ln.Factories
		}
	}
	if advCount > 0 {
		fcpu += int64(advCount) * adv.CPU
		fpg += int64(advCount) * adv.PG
		fparts = append(fparts, fmt.Sprintf("%d× Advanced Industry Facility (P2/P3)", advCount))
	}
	if highCount > 0 {
		fcpu += int64(highCount) * high.CPU
		fpg += int64(highCount) * high.PG
		fparts = append(fparts, fmt.Sprintf("%d× High-Tech Production Plant (P4, Barren/Temperate only)", highCount))
	}
	fcpu += launch.CPU
	fpg += launch.PG
	fparts = append(fparts, "1× Launchpad (import P1, export product)")
	// Split factory across planets when it cannot fit one CC.
	if fcpu > cc.CPU || fpg > cc.PG {
		// Rough split count by the tighter resource.
		byCPU, byPG := 1, 1
		if cc.CPU > 0 {
			byCPU = int(math.Ceil(float64(fcpu) / float64(cc.CPU)))
		}
		if cc.PG > 0 {
			byPG = int(math.Ceil(float64(fpg) / float64(cc.PG)))
		}
		n := byCPU
		if byPG > n {
			n = byPG
		}
		if n < 2 {
			n = 2
		}
		out = append(out, ColonyPlan{
			Role: "Factory", Planet: factPlanet,
			Title:   fmt.Sprintf("Factory ×%d planets", n),
			Detail:  fmt.Sprintf("Combined factory load exceeds one level-%d command center — split across %d factory planets (e.g. P2 on one, P3/P4 on another).", cc.Level, n),
			CPUUsed: fcpu, PGUsed: fpg, CPUCap: cc.CPU * int64(n), PGCap: cc.PG * int64(n),
			Fits: true, Breakdown: fparts,
		})
	} else {
		out = append(out, ColonyPlan{
			Role: "Factory", Planet: factPlanet,
			Title:   "Factory planet",
			Detail:  "Import P1 (and any P1 input to P4), run Advanced/High-Tech factories, export the final product.",
			CPUUsed: fcpu, PGUsed: fpg, CPUCap: cc.CPU, PGCap: cc.PG,
			Fits: true, Breakdown: fparts,
		})
	}
	return out
}

// FactoryPlanetOnly returns true when a facility kind is
// restricted: High-Tech only on Barren/Temperate.
func FactoryPlanetOnly(f FacilityKind) string {
	if f == FacilityHighTech {
		return "Barren, Temperate"
	}
	return "Any"
}

// Format helpers kept in engine so handler and tests share them.

// AllTargets lists selectable end products (P2-P4 plus P1), sorted
// P4 first for the picker default.
func AllTargets() []string {
	var out []string
	for _, t := range []Tier{TierP4, TierP3, TierP2, TierP1} {
		out = append(out, ByTier(t)...)
	}
	return out
}
