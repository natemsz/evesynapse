package piplan

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// realize turns the integer program's counts into colonies on real
// planets, run by real characters, with factories assigned and goods
// matched between planets.
func realize(sol *Solution, req Request, classes []*planetClass, zs []zref, keepRuns []keepRun,
	aVar, runVar, buyVar, sellVar map[string]int, x []float64) {

	// Character slots and the planets each already uses.
	slotsLeft := map[int64]int{}
	onPlanet := map[int64]map[int64]bool{}
	charName := map[int64]string{}
	var charOrder []int64
	for i, ch := range req.Characters {
		id := ch.ID
		if id == 0 {
			id = int64(-(i + 1))
		}
		slotsLeft[id] = max(ch.MaxPlanets, 1)
		onPlanet[id] = map[int64]bool{}
		charName[id] = ch.Name
		charOrder = append(charOrder, id)
	}

	type instance struct {
		class *planetClass
		col   column
		zv    int
		cand  int // -1: no real planet
		char  int64
	}
	var insts []instance
	// Existing colonies first: their planet and pilot are fixed.
	for _, z := range zs {
		n := int(math.Round(x[z.v]))
		cl := classes[z.class]
		for k := 0; k < n; k++ {
			in := instance{class: cl, col: cl.cols[z.col], zv: z.v, cand: -1}
			if cl.existing != nil {
				in.cand = cl.cands[0]
				if _, known := slotsLeft[cl.ownerID]; known {
					in.char = cl.ownerID
					slotsLeft[cl.ownerID]--
					onPlanet[cl.ownerID][req.Candidates[in.cand].PlanetID] = true
				}
			}
			insts = append(insts, in)
		}
	}
	// Existing colonies without a known pilot take any free slot, once
	// the owned ones hold theirs.
	for i := range insts {
		in := &insts[i]
		if in.class.existing == nil {
			continue
		}
		if _, known := slotsLeft[in.class.ownerID]; known {
			continue
		}
		pid := req.Candidates[in.cand].PlanetID
		for _, ch := range charOrder {
			if slotsLeft[ch] > 0 && (pid == 0 || !onPlanet[ch][pid]) {
				in.char = ch
				slotsLeft[ch]--
				if pid != 0 {
					onPlanet[ch][pid] = true
				}
				break
			}
		}
	}
	// Fresh colonies: nearest, smallest planets first; extraction
	// colonies choose first since their planet type is fixed.
	sort.SliceStable(insts, func(i, j int) bool {
		ri, rj := insts[i].col.adv+insts[i].col.ht > 0, insts[j].col.adv+insts[j].col.ht > 0
		return !ri && rj
	})
	for i := range insts {
		in := &insts[i]
		if in.class.existing != nil {
			continue
		}
		cands := append([]int(nil), in.class.cands...)
		sort.SliceStable(cands, func(a, b int) bool {
			ca, cb := req.Candidates[cands[a]], req.Candidates[cands[b]]
			if ca.Jumps != cb.Jumps {
				return ca.Jumps < cb.Jumps
			}
			return ca.RadiusKm < cb.RadiusKm
		})
		placed := false
		for _, ci := range cands {
			pid := req.Candidates[ci].PlanetID
			for _, ch := range charOrder {
				if slotsLeft[ch] > 0 && !onPlanet[ch][pid] {
					in.cand, in.char = ci, ch
					slotsLeft[ch]--
					onPlanet[ch][pid] = true
					placed = true
					break
				}
			}
			if placed {
				break
			}
		}
		if !placed {
			for _, ch := range charOrder {
				if slotsLeft[ch] > 0 {
					in.char = ch
					slotsLeft[ch]--
					break
				}
			}
		}
	}

	// Factory work: hand each factory colony its schematics.
	need := map[string]int{}
	for prod, v := range aVar {
		if n := int(math.Round(x[v])); n > 0 {
			need[prod] = n
		}
	}
	assign := make([]map[string]int, len(insts))
	for _, kind := range []FacilityKind{FacilityHighTech, FacilityAdvanced} {
		var prods []string
		for p := range need {
			if Schematics[p].Facility == kind {
				prods = append(prods, p)
			}
		}
		// Highest tier first, so a P3 line sits beside its P2 inputs.
		sort.Slice(prods, func(i, j int) bool {
			ti, tj := TierRank(TierOf(prods[i])), TierRank(TierOf(prods[j]))
			if ti != tj {
				return ti > tj
			}
			return prods[i] < prods[j]
		})
		for i := range insts {
			free := insts[i].col.adv
			if kind == FacilityHighTech {
				free = insts[i].col.ht
			}
			for _, p := range prods {
				if free == 0 {
					break
				}
				take := min(free, need[p])
				if take > 0 {
					if assign[i] == nil {
						assign[i] = map[string]int{}
					}
					assign[i][p] += take
					need[p] -= take
					free -= take
				}
			}
		}
	}

	runs := map[int]map[string]float64{}
	for _, k := range keepRuns {
		if runs[k.z] == nil {
			runs[k.z] = map[string]float64{}
		}
		runs[k.z][k.product] += x[k.v]
	}

	for i, in := range insts {
		pc := PlannedColony{Index: len(sol.Colonies), Role: in.col.role, Assumed: in.col.usesAs}
		cl := in.class
		radius := cl.radius
		if in.cand >= 0 {
			cand := req.Candidates[in.cand]
			pc.PlanetID, pc.PlanetName, pc.System, pc.Jumps = cand.PlanetID, cand.Name, cand.System, cand.Jumps
			if cand.RadiusKm > 0 {
				radius = cand.RadiusKm
			}
		} else {
			pc.PlanetName = "Any " + cl.ptype + " planet"
		}
		pc.PlanetType = cl.ptype
		pc.CharID = in.char
		pc.CharName = charName[in.char]
		pc.Existing = cl.existing != nil
		pc.Out, pc.In = map[string]float64{}, map[string]float64{}

		spec := in.col.spec
		switch {
		case in.col.keep:
			pc.Colony = cl.existing
			for p, q := range in.col.out {
				pc.Out[p] += q
			}
			for prod, run := range runs[in.zv] {
				s := Schematics[prod]
				rate := 3600 / float64(s.CycleSec)
				pc.Out[prod] += run * float64(s.OutputQty) * rate
				for inp, per := range s.Inputs {
					pc.In[inp] += run * float64(per) * rate
				}
			}
			pc.Summary = "Keep as it stands."
		case in.col.adv+in.col.ht > 0:
			spec.Factories = nil
			for _, p := range sortedKeys(assign[i]) {
				spec.Factories = append(spec.Factories, FactorySpec{Product: p, Count: assign[i][p]})
				s := Schematics[p]
				// The share of the built factories that runs.
				share := 1.0
				if built := x[aVar[p]]; built > 0 {
					share = math.Min(1, x[runVar[p]]/math.Round(built))
				}
				rate := 3600 / float64(s.CycleSec) * float64(assign[i][p]) * share
				pc.Out[p] += rate * float64(s.OutputQty)
				for inp, per := range s.Inputs {
					pc.In[inp] += rate * float64(per)
				}
			}
			// Storage to buffer one collection interval of goods.
			m3h := 0.0
			for it, q := range pc.In {
				m3h += q * AllCommodities[it].Volume
			}
			for it, q := range pc.Out {
				m3h += q * AllCommodities[it].Volume
			}
			spec.Launchpads, spec.Storages = 1, 0
			if extra := m3h*req.HaulHours - PinCapacity[PinLaunchpad]; extra > 0 {
				spec.Storages = int(math.Ceil(extra / PinCapacity[PinStorage]))
			}
			pc.Role = "Factory"
			pc.Summary = factorySummary(spec)
			if len(spec.Factories) == 0 {
				continue // the search left this one without work
			}
		default:
			for p, q := range in.col.out {
				pc.Out[p] += q
			}
			pc.Summary = extractionSummary(spec, pc.Out)
		}
		if pc.Colony == nil {
			spec.Name = fmt.Sprintf("%s: %s", cl.ptype, pc.Role)
			if cl.existing != nil {
				pc.Colony = buildColonyAround(cl.ptype, radius, cl.cc, spec, cl.existing)
			} else {
				pc.Colony = BuildColony(cl.ptype, radius, cl.cc, spec)
			}
			// A planet larger than the class's sizing one pays more for
			// its links: trim heads until it fits.
			for tries := 0; !pc.Colony.Fit().Fits() && tries < 20; tries++ {
				if !trimHead(pc.Colony) {
					break
				}
			}
		}
		pc.Colony.PlanetID, pc.Colony.PlanetName = pc.PlanetID, pc.PlanetName
		pc.Colony.CCLevel = max(pc.Colony.CCLevel, cl.cc)
		pc.Fit = pc.Colony.Fit()
		for _, p := range pc.Colony.Pins {
			switch p.Keep {
			case ChangeAdd:
				pc.Added++
			case ChangeRemove:
				pc.Removed++
			case ChangeRetool:
				pc.Retooled++
			}
		}
		if !pc.Fit.Fits() {
			sol.Notes = append(sol.Notes, fmt.Sprintf("%s does not fit its command center as laid out; drop a factory or a head there.", pc.PlanetName))
		}
		sol.Colonies = append(sol.Colonies, pc)
	}
	sol.SlotsUsed = len(sol.Colonies)

	// Net each colony's own use of what it makes before matching.
	for i := range sol.Colonies {
		pc := &sol.Colonies[i]
		for it, q := range pc.In {
			if o := pc.Out[it]; o > 0 {
				use := math.Min(o, q)
				pc.Out[it] -= use
				pc.In[it] -= use
			}
		}
		clean(pc.Out)
		clean(pc.In)
	}
	sol.Flows = matchFlows(sol.Colonies, buyVar, sellVar, x)
	for _, f := range sol.Flows {
		sol.HaulM3Day += f.M3Day
	}
}

func clean(m map[string]float64) {
	for k, v := range m {
		if v < 1e-6 {
			delete(m, k)
		}
	}
}

// trimHead drops one head from the extractor with the most; false
// when no extractor has more than one.
func trimHead(c *Colony) bool {
	best := -1
	for i, p := range c.Pins {
		if p.Kind == PinExtractor && p.Keep != ChangeRemove && p.Heads > 1 && (best < 0 || p.Heads > c.Pins[best].Heads) {
			best = i
		}
	}
	if best < 0 {
		return false
	}
	c.Pins[best].Heads--
	linkAndRoute(c)
	return true
}

// matchFlows pairs what colonies send out with what others need,
// market purchases filling gaps and sales taking the rest.
func matchFlows(cols []PlannedColony, buyVar, sellVar map[string]int, x []float64) []Flow {
	type lot struct {
		at  int
		qty float64
	}
	supply, demand := map[string][]lot{}, map[string][]lot{}
	for i, c := range cols {
		for it, q := range c.Out {
			supply[it] = append(supply[it], lot{i, q})
		}
		for it, q := range c.In {
			demand[it] = append(demand[it], lot{i, q})
		}
	}
	for it, v := range buyVar {
		if q := x[v]; q > 1e-6 {
			supply[it] = append(supply[it], lot{-1, q})
		}
	}
	var flows []Flow
	add := func(from, to int, it string, perHour float64) {
		if perHour < 1e-6 {
			return
		}
		flows = append(flows, Flow{From: from, To: to, Item: it, PerDay: perHour * 24, M3Day: perHour * 24 * AllCommodities[it].Volume})
	}
	for _, it := range sortedKeys(supply) {
		sup := supply[it]
		dem := demand[it]
		di := 0
		for si := range sup {
			for sup[si].qty > 1e-6 && di < len(dem) {
				take := math.Min(sup[si].qty, dem[di].qty)
				add(sup[si].at, dem[di].at, it, take)
				sup[si].qty -= take
				dem[di].qty -= take
				if dem[di].qty <= 1e-6 {
					di++
				}
			}
			if sup[si].qty > 1e-6 && sup[si].at >= 0 {
				add(sup[si].at, -1, it, sup[si].qty) // to market
			}
		}
	}
	sort.SliceStable(flows, func(i, j int) bool {
		if flows[i].From != flows[j].From {
			return flows[i].From < flows[j].From
		}
		return flows[i].Item < flows[j].Item
	})
	return flows
}

func factorySummary(spec ColonySpec) string {
	var parts []string
	for _, f := range spec.Factories {
		parts = append(parts, fmt.Sprintf("%d × %s", f.Count, f.Product))
	}
	s := "Factories: " + strings.Join(parts, ", ")
	if spec.Storages > 0 {
		s += fmt.Sprintf("; %d storage", spec.Storages)
	}
	return s + "."
}

func extractionSummary(spec ColonySpec, out map[string]float64) string {
	byRaw := map[string][]int{}
	for _, e := range spec.ECUs {
		byRaw[e.Raw] = append(byRaw[e.Raw], e.Heads)
	}
	var parts []string
	for _, raw := range sortedKeys(byRaw) {
		h := byRaw[raw]
		total := 0
		for _, v := range h {
			total += v
		}
		unit := "extractor"
		if len(h) > 1 {
			unit = "extractors"
		}
		parts = append(parts, fmt.Sprintf("%d %s on %s (%d heads)", len(h), unit, raw, total))
	}
	s := strings.Join(parts, ", ")
	var made []string
	for _, f := range spec.Factories {
		made = append(made, fmt.Sprintf("%d × %s", f.Count, f.Product))
	}
	if len(made) > 0 {
		s += "; " + strings.Join(made, ", ")
	}
	return s + "."
}
