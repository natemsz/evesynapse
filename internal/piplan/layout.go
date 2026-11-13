package piplan

import (
	"math"
	"sort"
)

// ColonySpec is what a colony must hold; BuildColony places it.
type ColonySpec struct {
	Name       string
	ECUs       []ECUSpec
	Factories  []FactorySpec
	Launchpads int
	Storages   int
}

// ECUSpec is one extractor control unit.
type ECUSpec struct {
	Raw          string
	Heads        int
	ProgramHours float64
	PeakPerHead  float64 // per-head base per hour, for the route size
	QtyPerCycle  float64 // measured base, kept when reusing a unit
}

// FactorySpec is a run of identical factories.
type FactorySpec struct {
	Product string
	Count   int
}

// gridStep is the spacing between neighbouring structures, a hair
// over the game's minimum so a template imports without moves.
const gridStep = 0.0125

type hexCell struct {
	q, r int
	ring int
}

// hexCells lists the cells of a hex grid out to the given ring,
// nearest first.
func hexCells(rings int) []hexCell {
	var out []hexCell
	for q := -rings; q <= rings; q++ {
		for r := -rings; r <= rings; r++ {
			s := -q - r
			ring := max(abs(q), abs(r), abs(s))
			if ring <= rings {
				out = append(out, hexCell{q, r, ring})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ring != out[j].ring {
			return out[i].ring < out[j].ring
		}
		ai := math.Atan2(cellXY(out[i]))
		aj := math.Atan2(cellXY(out[j]))
		return ai < aj
	})
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// cellXY is a cell's offset in grid steps (y first, for Atan2).
func cellXY(c hexCell) (float64, float64) {
	x := float64(c.q) + float64(c.r)/2
	y := float64(c.r) * math.Sqrt(3) / 2
	return y, x
}

// cellPos places a cell around a centre point on the planet.
func cellPos(center LatLon, c hexCell) LatLon {
	y, x := cellXY(c)
	return LatLon{Lat: center.Lat + y*gridStep, Lon: center.Lon + x*gridStep/math.Max(math.Sin(center.Lat), 0.2)}
}

// defaultCenter is where a fresh colony is drawn: on the equator.
var defaultCenter = LatLon{Lat: math.Pi / 2, Lon: 0}

// BuildColony lays a colony out from a spec: a launchpad hub with
// every other structure in hex rings around it, each linked to its
// nearest neighbour one ring in, so every route crosses at most a
// handful of structures. Routes run through the hub; links are
// upgraded where their load needs it.
func BuildColony(planetType string, radiusKm float64, ccLevel int, spec ColonySpec) *Colony {
	return buildColonyAround(planetType, radiusKm, ccLevel, spec, nil)
}

// buildColonyAround is BuildColony, reusing an existing colony's
// structures where the spec has a place for them. Reused pins keep
// their positions; new ones fill the free cells nearest the hub.
func buildColonyAround(planetType string, radiusKm float64, ccLevel int, spec ColonySpec, existing *Colony) *Colony {
	c := &Colony{PlanetType: planetType, RadiusKm: radiusKm, CCLevel: ccLevel, Name: spec.Name}
	if spec.Launchpads < 1 {
		spec.Launchpads = 1
	}
	// The wanted pins, hub first.
	type want struct {
		kind    PinKind
		product string
		ecu     *ECUSpec
	}
	var wants []want
	for i := 0; i < spec.Launchpads; i++ {
		wants = append(wants, want{kind: PinLaunchpad})
	}
	wants = append(wants, want{kind: PinCommandCenter})
	for i := 0; i < spec.Storages; i++ {
		wants = append(wants, want{kind: PinStorage})
	}
	for _, f := range spec.Factories {
		sch, ok := Schematics[f.Product]
		if !ok {
			continue
		}
		for i := 0; i < f.Count; i++ {
			wants = append(wants, want{kind: facilityPin(sch.Facility), product: f.Product})
		}
	}
	for i := range spec.ECUs {
		wants = append(wants, want{kind: PinExtractor, product: spec.ECUs[i].Raw, ecu: &spec.ECUs[i]})
	}

	center := defaultCenter
	used := map[int]bool{} // existing pin indexes taken
	if existing != nil {
		for _, p := range existing.Pins {
			if p.Kind == PinLaunchpad {
				center = p.pos()
				break
			} else if p.Kind == PinCommandCenter {
				center = p.pos()
			}
		}
	}
	// Match wants to existing pins: same kind and product first, then
	// same kind (a factory retooled).
	placed := make([]int, len(wants)) // index into c.Pins, -1 until placed
	for i := range placed {
		placed[i] = -1
	}
	if existing != nil {
		for pass := range 2 {
			for wi, w := range wants {
				if placed[wi] >= 0 {
					continue
				}
				for ei, p := range existing.Pins {
					if used[ei] || p.Kind != w.kind {
						continue
					}
					if pass == 0 && p.Product != w.product && w.kind != PinCommandCenter && w.kind != PinLaunchpad && w.kind != PinStorage {
						continue
					}
					used[ei] = true
					np := p
					np.Product = w.product
					np.Keep = ChangeKeep
					if pass == 1 && p.Product != w.product && w.product != "" {
						np.Keep = ChangeRetool
					}
					if w.ecu != nil {
						np.Heads = w.ecu.Heads
						np.ProgramHours = w.ecu.ProgramHours
						np.Peak = w.ecu.PeakPerHead
						if p.Product != w.product {
							np.HeadPos, np.QtyPerCycle = nil, 0
						}
					}
					c.Pins = append(c.Pins, np)
					placed[wi] = len(c.Pins) - 1
					break
				}
			}
		}
	}
	// Free cells for the rest.
	occupied := func(pos LatLon) bool {
		for _, p := range c.Pins {
			if Angle(p.pos(), pos) < gridStep*0.9 {
				return true
			}
		}
		if existing != nil {
			for ei, p := range existing.Pins {
				if !used[ei] && Angle(p.pos(), pos) < gridStep*0.9 {
					// a structure being torn down still stands until it goes;
					// keep clear of it so the plan can be built in place.
					return true
				}
			}
		}
		return false
	}
	cells := hexCells(6)
	ci := 0
	for wi, w := range wants {
		if placed[wi] >= 0 {
			continue
		}
		var pos LatLon
		for ci < len(cells) {
			pos = cellPos(center, cells[ci])
			ci++
			if !occupied(pos) {
				break
			}
		}
		p := Pin{Kind: w.kind, Lat: pos.Lat, Lon: pos.Lon, Product: w.product}
		if existing != nil {
			p.Keep = ChangeAdd
		}
		if w.ecu != nil {
			p.Heads = w.ecu.Heads
			p.ProgramHours = w.ecu.ProgramHours
			p.QtyPerCycle = w.ecu.QtyPerCycle
			p.Peak = w.ecu.PeakPerHead
		}
		c.Pins = append(c.Pins, p)
		placed[wi] = len(c.Pins) - 1
	}
	if existing != nil {
		for ei, p := range existing.Pins {
			if !used[ei] {
				p.Keep = ChangeRemove
				c.Pins = append(c.Pins, p)
			}
		}
	}
	linkAndRoute(c)
	return c
}

// linkAndRoute joins the live pins into a tree around the first
// launchpad and lays the routes: extractors and factories deliver to
// the hub, the hub feeds every factory its inputs.
func linkAndRoute(c *Colony) {
	c.Links, c.Routes = nil, nil
	hub := -1
	var live []int
	for i, p := range c.Pins {
		if p.Keep == ChangeRemove || p.Kind == PinCommandCenter {
			continue
		}
		live = append(live, i)
		if hub < 0 && p.Kind == PinLaunchpad {
			hub = i
		}
	}
	if hub < 0 {
		return
	}
	// Prim-style tree from the hub: each pin joins the nearest pin
	// already in the tree that is closer to the hub than it is.
	parent := map[int]int{hub: -1}
	order := append([]int(nil), live...)
	sort.SliceStable(order, func(a, b int) bool {
		return Angle(c.Pins[order[a]].pos(), c.Pins[hub].pos()) < Angle(c.Pins[order[b]].pos(), c.Pins[hub].pos())
	})
	for _, i := range order {
		if i == hub {
			continue
		}
		best, bestD := hub, math.Inf(1)
		for j := range parent {
			// Keep paths short: a pin may only hang off the hub or a
			// pin hanging off the hub directly or one step out.
			if depth(parent, j) >= 2 {
				continue
			}
			d := Angle(c.Pins[i].pos(), c.Pins[j].pos())
			if d < bestD {
				best, bestD = j, d
			}
		}
		parent[i] = best
		c.Links = append(c.Links, Link{A: best, B: i})
	}
	pathTo := func(i int) []int { // hub ... i
		var rev []int
		for k := i; k >= 0; k = parent[k] {
			rev = append(rev, k)
		}
		for a, b := 0, len(rev)-1; a < b; a, b = a+1, b-1 {
			rev[a], rev[b] = rev[b], rev[a]
		}
		return rev
	}
	reverse := func(p []int) []int {
		out := make([]int, len(p))
		for i := range p {
			out[i] = p[len(p)-1-i]
		}
		return out
	}
	load := map[[2]int]float64{} // m3/h per link
	addLoad := func(path []int, m3h float64) {
		for k := 1; k < len(path); k++ {
			a, b := path[k-1], path[k]
			if a > b {
				a, b = b, a
			}
			load[[2]int{a, b}] += m3h
		}
	}
	vol := func(name string) float64 { return AllCommodities[name].Volume }
	for _, i := range live {
		p := c.Pins[i]
		switch p.Kind {
		case PinExtractor:
			if p.Product == "" {
				continue
			}
			cyc := ProgramCycleSec(p.ProgramHours)
			perHour := ECUOutputPerHour(peakFor(p), p.Heads, p.ProgramHours)
			qty := max(int64(math.Round(perHour*float64(cyc)/3600)), 1)
			path := reverse(pathTo(i))
			c.Routes = append(c.Routes, Route{Path: path, Item: p.Product, Qty: qty})
			addLoad(path, perHour*vol(p.Product))
		case PinBasic, PinAdvanced, PinHighTech:
			sch, ok := Schematics[p.Product]
			if !ok {
				continue
			}
			cycH := float64(sch.CycleSec) / 3600
			in := pathTo(i)
			names := make([]string, 0, len(sch.Inputs))
			for n := range sch.Inputs {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				q := sch.Inputs[n]
				c.Routes = append(c.Routes, Route{Path: in, Item: n, Qty: q})
				addLoad(in, float64(q)/cycH*vol(n))
			}
			out := reverse(in)
			c.Routes = append(c.Routes, Route{Path: out, Item: p.Product, Qty: sch.OutputQty})
			addLoad(out, float64(sch.OutputQty)/cycH*vol(p.Product))
		}
	}
	for li, l := range c.Links {
		a, b := l.A, l.B
		if a > b {
			a, b = b, a
		}
		need := load[[2]int{a, b}]
		lvl := 0
		for lvl < MaxLinkLevel && LinkBandwidthAt(lvl) < need {
			lvl++
		}
		c.Links[li].Level = lvl
	}
}

func depth(parent map[int]int, i int) int {
	d := 0
	for k := parent[i]; k >= 0; k = parent[k] {
		d++
	}
	return d
}

// peakFor is a pin's per-head base rate per hour: measured when the
// pin carries ESI's figure, else the planner's assumption stored on
// the pin by the optimizer.
func peakFor(p Pin) float64 {
	if p.QtyPerCycle > 0 && p.Heads > 0 {
		return PeakPerHeadHour(p.QtyPerCycle, p.Heads)
	}
	if p.Peak > 0 {
		return p.Peak
	}
	return DefaultPeakPerHead
}

// DefaultPeakPerHead is the planner's assumed base rate per extractor
// head per hour when nothing better is known: a middling survey spot.
// Players replace it with their own survey figure, and colonies the
// game reports replace it with the measured one.
const DefaultPeakPerHead = 1500.0
