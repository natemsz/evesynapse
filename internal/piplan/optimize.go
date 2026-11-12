package piplan

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

var p1Once sync.Once

// The colony optimizer. Given what to make, who makes it and which
// planets are in reach, it decides which planet hosts which colony,
// what each colony extracts and builds and in what numbers, and what
// moves between planets. It can start from nothing or from the
// colonies a player already has, keeping what still serves.
//
// Shape of the model: every product has exactly one recipe, so the
// decisions are where work runs and how much capacity to build.
// Planets are grouped into classes (one per planet type for fresh
// planets; each existing colony its own class). Each class offers a
// handful of colony designs ("columns"), each sized exactly by
// laying it out and fitting it. A small integer program picks how
// many of each design to build and how many factories run each
// schematic; the result is then placed on real planets.

// Character is one pilot's planetary allowance.
type Character struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	MaxPlanets int    `json:"maxPlanets"` // 1 + Interplanetary Consolidation
	CCLevel    int    `json:"ccLevel"`    // Command Center Upgrades
}

// Candidate is one planet the plan may use.
type Candidate struct {
	PlanetID int64   `json:"planetId"`
	Name     string  `json:"name"`
	Type     string  `json:"type"`
	System   string  `json:"system"`
	Jumps    int     `json:"jumps"`
	RadiusKm float64 `json:"radiusKm"`
	// Existing is the colony a character already runs here (adapt
	// mode keeps what still serves); OwnerID is that character.
	Existing *Colony `json:"-"`
	OwnerID  int64   `json:"ownerId,omitempty"`
	// Peak is a per-head base rate per hour per raw, measured from a
	// colony on this planet or entered from an in-game survey.
	Peak map[string]float64 `json:"-"`
	// Fixed marks Existing as a colony the player laid out by hand:
	// the plan keeps it exactly as it stands, in every mode. OwnerID
	// may be 0, and then any pilot with a free slot runs it.
	Fixed bool `json:"-"`
}

// Request is one optimization.
type Request struct {
	Target       string
	PerDay       float64 // units of Target a day; 0 = as much as the planets allow
	Characters   []Character
	Candidates   []Candidate
	Peak         float64 // assumed per-head base rate per hour
	ProgramHours float64 // extractor program length (how often extractors are reset)
	HaulHours    float64 // how often goods are collected and delivered
	TaxRate      float64 // customs office tax, fraction
	Prices       map[string]float64
	AllowBuy     bool // buy intermediates instead of making them, where cheaper
	SellSurplus  bool // sell leftovers
	Adapt        bool // keep existing colonies where they serve
}

// PlannedColony is one colony of a solution.
type PlannedColony struct {
	Index      int                `json:"index"`
	Role       string             `json:"role"`
	Summary    string             `json:"summary"`
	PlanetID   int64              `json:"planetId,omitempty"`
	PlanetName string             `json:"planetName"`
	PlanetType string             `json:"planetType"`
	System     string             `json:"system,omitempty"`
	Jumps      int                `json:"jumps"`
	CharID     int64              `json:"charId,omitempty"`
	CharName   string             `json:"charName,omitempty"`
	Existing   bool               `json:"existing"`
	Colony     *Colony            `json:"colony"`
	Fit        Fit                `json:"fit"`
	Out        map[string]float64 `json:"out"` // units per hour leaving the planet
	In         map[string]float64 `json:"in"`  // units per hour arriving
	Added      int                `json:"added"`
	Removed    int                `json:"removed"`
	Retooled   int                `json:"retooled"`
	Assumed    bool               `json:"assumed"` // extraction rests on the assumed rate
}

// Flow is goods moving between colonies (or to market).
type Flow struct {
	From   int     `json:"from"` // colony index, -1 = market
	To     int     `json:"to"`   // colony index, -1 = market
	Item   string  `json:"item"`
	PerDay float64 `json:"perDay"`
	M3Day  float64 `json:"m3Day"`
}

// Solution is the optimizer's answer.
type Solution struct {
	Target       string          `json:"target"`
	PerDay       float64         `json:"perDay"` // target made per day
	Colonies     []PlannedColony `json:"colonies"`
	Flows        []Flow          `json:"flows"`
	RevenueDay   float64         `json:"revenueDay"`
	BuyDay       float64         `json:"buyDay"`
	TaxDay       float64         `json:"taxDay"`
	ProfitDay    float64         `json:"profitDay"`
	PricesKnown  bool            `json:"pricesKnown"`
	HaulM3Day    float64         `json:"haulM3Day"`
	SlotsUsed    int             `json:"slotsUsed"`
	SlotsTotal   int             `json:"slotsTotal"`
	Exact        bool            `json:"exact"`
	Notes        []string        `json:"notes"`
	Short        string          `json:"short,omitempty"` // why a target could not be met
	ProgramHours float64         `json:"programHours"`
}

// TypicalRadiusKm is the median planet radius per type in the SDE
// (mapDenormalize, 2026-11-12), used when no real planet is named.
var TypicalRadiusKm = map[string]float64{
	"Temperate": 6440, "Ice": 14600, "Gas": 26370, "Oceanic": 4365,
	"Lava": 3360, "Barren": 3750, "Storm": 6530, "Plasma": 3120,
}

// --- columns ----------------------------------------------------------------

type column struct {
	label  string
	spec   ColonySpec
	out    map[string]float64 // net units per hour produced for export
	adv    int                // free advanced factory slots
	ht     int                // free high-tech slots
	fixed  map[string]int     // existing factories pinned to a schematic (keep column)
	keep   bool
	edits  float64 // structures changed against the existing colony
	role   string
	usesAs bool // extraction rests on the assumed rate
}

type planetClass struct {
	key      string
	ptype    string
	radius   float64
	cc       int
	count    int // colonies this class can host
	cands    []int
	existing *Colony
	fixed    bool // existing is kept as it stands: no other design is offered
	ownerID  int64
	peak     map[string]float64
	cols     []column
}

// basicRaw is a basic factory's raw use and P1 output per hour.
const (
	basicRawPerHour = 6000.0
	basicP1PerHour  = 40.0
)

func (r *Request) peakFor(cl *planetClass, raw string) (float64, bool) {
	if v, ok := cl.peak[raw]; ok && v > 0 {
		return v, false
	}
	if r.Peak > 0 {
		return r.Peak, true
	}
	return DefaultPeakPerHead, true
}

// extractionColumn sizes a colony that extracts the given raws (one
// entry per extractor unit) and refines them to P1 on the planet;
// with p2 set, advanced factories on the planet turn the P1 into it.
func (r *Request) extractionColumn(cl *planetClass, raws []string, p2 string) (column, bool) {
	for heads := MaxHeads; heads >= 2; heads-- {
		spec := ColonySpec{Launchpads: 1}
		rawRate := map[string]float64{}
		assumed := false
		for _, raw := range raws {
			pk, as := r.peakFor(cl, raw)
			assumed = assumed || as
			spec.ECUs = append(spec.ECUs, ECUSpec{Raw: raw, Heads: heads, ProgramHours: r.ProgramHours, PeakPerHead: pk})
			rawRate[raw] += ECUOutputPerHour(pk, heads, r.ProgramHours)
		}
		out := map[string]float64{}
		names := sortedKeys(rawRate)
		for _, raw := range names {
			p1, ok := p1ForRaw[raw]
			if !ok {
				return column{}, false
			}
			n := int(math.Ceil(rawRate[raw]/basicRawPerHour - 1e-9))
			spec.Factories = append(spec.Factories, FactorySpec{Product: p1, Count: n})
			out[p1] += rawRate[raw] / basicRawPerHour * basicP1PerHour
		}
		role := "Extract and refine"
		if p2 != "" {
			sch := Schematics[p2]
			// As many factories as the scarcer input feeds.
			n := math.MaxInt
			for in, per := range sch.Inputs {
				perHour := float64(per) * 3600 / float64(sch.CycleSec)
				n = min(n, int(math.Floor(out[in]/perHour+1e-9)))
			}
			if n < 1 {
				return column{}, false
			}
			spec.Factories = append(spec.Factories, FactorySpec{Product: p2, Count: n})
			for in, per := range sch.Inputs {
				out[in] -= float64(n) * float64(per) * 3600 / float64(sch.CycleSec)
				if out[in] < 1e-6 {
					delete(out, in)
				}
			}
			out[p2] += float64(n) * float64(sch.OutputQty) * 3600 / float64(sch.CycleSec)
			role = "Extract and make " + p2
		}
		spec.Name = cl.ptype + ": " + strings.Join(dedupe(raws), " + ")
		col := BuildColony(cl.ptype, cl.radius, cl.cc, spec)
		if !col.Fit().Fits() {
			continue
		}
		return column{label: spec.Name, spec: spec, out: out, role: role, usesAs: assumed}, true
	}
	return column{}, false
}

// factoryColumns are the factory colonies a class can host: for each
// high-tech count up to htLimit, the most advanced factories that
// still fit, with storage enough to buffer a collection interval.
func (r *Request) factoryColumns(cl *planetClass, advProduct, htProduct string, htLimit int) []column {
	var cols []column
	maxHT := 0
	if htProduct != "" && CanHold(cl.ptype, PinHighTech) {
		maxHT = htLimit
	}
	for ht := 0; ht <= maxHT; ht++ {
		best := -1
		var bestSpec ColonySpec
		for adv := 0; adv <= 40; adv++ {
			if adv+ht == 0 {
				continue
			}
			spec := ColonySpec{Launchpads: 1}
			// Buffer: each factory moves about 20 m3 an hour in and out.
			need := float64(adv+ht) * 20 * r.HaulHours
			if extra := need - PinCapacity[PinLaunchpad]; extra > 0 {
				spec.Storages = int(math.Ceil(extra / PinCapacity[PinStorage]))
			}
			if adv > 0 && advProduct != "" {
				spec.Factories = append(spec.Factories, FactorySpec{Product: advProduct, Count: adv})
			}
			if ht > 0 {
				spec.Factories = append(spec.Factories, FactorySpec{Product: htProduct, Count: ht})
			}
			if !BuildColony(cl.ptype, cl.radius, cl.cc, spec).Fit().Fits() {
				break
			}
			best, bestSpec = adv, spec
		}
		if best < 0 {
			break
		}
		if advProduct == "" && best > 0 {
			continue
		}
		cols = append(cols, column{
			label: fmt.Sprintf("%s factory: %d advanced, %d high-tech", cl.ptype, best, ht),
			spec:  bestSpec, adv: best, ht: ht, role: "Factory",
		})
	}
	// Keep the Pareto front only.
	sort.Slice(cols, func(i, j int) bool { return cols[i].ht < cols[j].ht })
	var front []column
	for i, c := range cols {
		dominated := false
		for j, d := range cols {
			if i != j && d.adv >= c.adv && d.ht >= c.ht && (d.adv > c.adv || d.ht > c.ht) {
				dominated = true
				break
			}
		}
		if !dominated {
			front = append(front, c)
		}
	}
	return front
}

// keepColumn describes an existing colony as it stands. Only what
// the target's chain uses counts: factories and extractors on other
// products stay on the planet and add nothing to the plan.
func (r *Request) keepColumn(cl *planetClass, inChain map[string]bool) column {
	col := column{label: "Keep as is", keep: true, role: "Keep", out: map[string]float64{}, fixed: map[string]int{}}
	rawRate := map[string]float64{}
	basics := map[string]int{}
	for _, p := range cl.existing.Pins {
		if p.Keep == ChangeRemove {
			continue
		}
		switch p.Kind {
		case PinExtractor:
			if p.Product == "" || p.Heads <= 0 {
				continue
			}
			// Best figure first: the game's measured cycle, the survey
			// value on the pin, the planet's measured rate, the
			// request's assumption.
			var pk float64
			switch {
			case p.QtyPerCycle > 0:
				pk = PeakPerHeadHour(p.QtyPerCycle, p.Heads)
			case p.Peak > 0:
				pk = p.Peak
				// A pin carrying the plain assumption is still assumed.
				if def, _ := r.peakFor(&planetClass{}, p.Product); pk == def && cl.peak[p.Product] <= 0 {
					col.usesAs = true
				}
			default:
				var assumed bool
				pk, assumed = r.peakFor(cl, p.Product)
				col.usesAs = col.usesAs || assumed
			}
			hrs := p.ProgramHours
			if hrs <= 0 {
				hrs = r.ProgramHours
			}
			rawRate[p.Product] += ECUOutputPerHour(pk, p.Heads, hrs)
		case PinBasic:
			basics[p.Product]++
		case PinAdvanced, PinHighTech:
			if _, ok := Schematics[p.Product]; ok && inChain[p.Product] {
				col.fixed[p.Product]++
			}
		}
	}
	for _, raw := range sortedKeys(rawRate) {
		p1, ok := p1ForRaw[raw]
		if !ok || !inChain[p1] {
			continue
		}
		used := math.Min(rawRate[raw], float64(basics[p1])*basicRawPerHour)
		if used > 0 {
			col.out[p1] += used / basicRawPerHour * basicP1PerHour
		}
	}
	col.spec = specOf(cl.existing)
	return col
}

// specOf reads a colony back into a spec.
func specOf(c *Colony) ColonySpec {
	spec := ColonySpec{Name: c.Name}
	count := map[string]int{}
	for _, p := range c.Pins {
		switch p.Kind {
		case PinLaunchpad:
			spec.Launchpads++
		case PinStorage:
			spec.Storages++
		case PinExtractor:
			spec.ECUs = append(spec.ECUs, ECUSpec{Raw: p.Product, Heads: p.Heads, ProgramHours: p.ProgramHours, QtyPerCycle: p.QtyPerCycle, PeakPerHead: p.Peak})
		case PinBasic, PinAdvanced, PinHighTech:
			count[p.Product]++
		}
	}
	for _, k := range sortedKeys(count) {
		spec.Factories = append(spec.Factories, FactorySpec{Product: k, Count: count[k]})
	}
	return spec
}

// editsBetween counts the structures a column adds or removes
// against an existing colony.
func editsBetween(existing *Colony, spec ColonySpec) float64 {
	have := map[string]int{}
	for _, p := range existing.Pins {
		if p.Kind == PinCommandCenter {
			continue
		}
		have[string(p.Kind)+"|"+p.Product]++
	}
	want := map[string]int{}
	want[string(PinLaunchpad)+"|"] += max(spec.Launchpads, 1)
	want[string(PinStorage)+"|"] += spec.Storages
	for _, e := range spec.ECUs {
		want[string(PinExtractor)+"|"+e.Raw]++
	}
	for _, f := range spec.Factories {
		want[string(facilityPin(Schematics[f.Product].Facility))+"|"+f.Product] += f.Count
	}
	d := 0.0
	keys := map[string]bool{}
	for k := range have {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	for k := range keys {
		d += math.Abs(float64(have[k] - want[k]))
	}
	return d / 2
}

// p1ForRaw maps each raw to the P1 made from it. Filled on first use:
// Schematics is built in init, after package variables.
var p1ForRaw = map[string]string{}

func fillP1ForRaw() {
	if len(p1ForRaw) > 0 {
		return
	}
	for name, s := range Schematics {
		if s.Facility == FacilityBasic {
			for in := range s.Inputs {
				p1ForRaw[in] = name
			}
		}
	}
}

// --- solve ------------------------------------------------------------------

// Optimize builds and solves the model.
func Optimize(req Request) (*Solution, error) {
	p1Once.Do(fillP1ForRaw)
	if _, ok := Schematics[req.Target]; !ok {
		return nil, fmt.Errorf("pick a P1 to P4 product")
	}
	if req.ProgramHours <= 0 {
		req.ProgramHours = 24
	}
	if req.HaulHours <= 0 {
		req.HaulHours = 24
	}
	if len(req.Characters) == 0 {
		req.Characters = []Character{{Name: "Pilot", MaxPlanets: 6, CCLevel: 5}}
	}
	chain := ChainNodes(req.Target)
	inChain := map[string]bool{}
	for _, c := range chain {
		inChain[c] = true
	}
	needRaw := map[string]bool{}
	for _, c := range chain {
		if TierOf(c) == TierRaw {
			needRaw[c] = true
		}
	}

	slots, ccMin := 0, 5
	for _, ch := range req.Characters {
		slots += max(ch.MaxPlanets, 1)
		ccMin = min(ccMin, ch.CCLevel)
	}
	nChars := len(req.Characters)

	// Classes.
	var classes []*planetClass
	byType := map[string]*planetClass{}
	held := 0 // slots that standing colonies occupy
	for i, cand := range req.Candidates {
		if cand.Existing != nil && (req.Adapt || cand.Fixed) {
			cc := cand.Existing.CCLevel
			for _, ch := range req.Characters {
				if ch.ID == cand.OwnerID && ch.CCLevel > cc {
					cc = ch.CCLevel
				}
			}
			classes = append(classes, &planetClass{
				key: fmt.Sprintf("p%d", cand.PlanetID), ptype: cand.Type, radius: cand.RadiusKm, cc: cc,
				count: 1, cands: []int{i}, existing: cand.Existing, fixed: cand.Fixed, ownerID: cand.OwnerID, peak: cand.Peak,
			})
			held++
			continue
		}
		if len(cand.Peak) > 0 {
			classes = append(classes, &planetClass{
				key: fmt.Sprintf("p%d", cand.PlanetID), ptype: cand.Type, radius: cand.RadiusKm, cc: ccMin,
				count: nChars, cands: []int{i}, peak: cand.Peak,
			})
			continue
		}
		cl := byType[cand.Type]
		if cl == nil {
			cl = &planetClass{key: "t" + cand.Type, ptype: cand.Type, radius: cand.RadiusKm, cc: ccMin}
			byType[cand.Type] = cl
			classes = append(classes, cl)
		}
		cl.cands = append(cl.cands, i)
		cl.count += nChars
		if cand.RadiusKm > 0 && cand.RadiusKm < cl.radius {
			cl.radius = cand.RadiusKm
		}
	}
	free := max(slots-held, 0)
	if len(classes) == held && free > 0 {
		// No planets offered beyond the standing colonies: one class
		// per type at a typical size fills the slots left.
		for _, pt := range PlanetTypes {
			classes = append(classes, &planetClass{key: "t" + pt, ptype: pt, radius: TypicalRadiusKm[pt], cc: ccMin, count: free})
		}
	}

	// Representative products for sizing factory colonies.
	advProduct, htProduct := "", ""
	for _, c := range chain {
		if s, ok := Schematics[c]; ok {
			if s.Facility == FacilityAdvanced && (advProduct == "" || TierOf(c) == TierP3) {
				advProduct = c
			}
			if s.Facility == FacilityHighTech {
				htProduct = c
			}
		}
	}

	// Columns per class.
	for _, cl := range classes {
		if cl.radius <= 0 {
			cl.radius = TypicalRadiusKm[cl.ptype]
		}
		if cl.fixed {
			// A hand-laid colony offers one design: itself.
			cl.cols = []column{req.keepColumn(cl, inChain)}
			continue
		}
		var local []string
		for _, raw := range PlanetYields[cl.ptype] {
			if needRaw[raw] {
				local = append(local, raw)
			}
		}
		for _, raw := range local {
			for _, n := range []int{1, 2} {
				raws := make([]string, n)
				for k := range raws {
					raws[k] = raw
				}
				if c, ok := req.extractionColumn(cl, raws, ""); ok {
					cl.cols = append(cl.cols, c)
				}
			}
		}
		for a := 0; a < len(local); a++ {
			for b := a + 1; b < len(local); b++ {
				if c, ok := req.extractionColumn(cl, []string{local[a], local[b]}, ""); ok {
					cl.cols = append(cl.cols, c)
				}
			}
		}
		// One planet, two raws, straight to P2.
		for _, c := range chain {
			s, ok := Schematics[c]
			if !ok || TierOf(c) != TierP2 {
				continue
			}
			var raws []string
			for in := range s.Inputs {
				for _, raw := range local {
					if p1ForRaw[raw] == in {
						raws = append(raws, raw)
					}
				}
			}
			if len(raws) == len(s.Inputs) {
				sort.Strings(raws)
				if col, ok := req.extractionColumn(cl, raws, c); ok {
					cl.cols = append(cl.cols, col)
				}
			}
		}
		if cl.existing != nil {
			cl.cols = append(cl.cols, req.keepColumn(cl, inChain))
		}
	}
	// Factory colonies. Without buying, the high-tech factories worth
	// offering are capped by what every slot extracting flat out
	// could feed.
	emb := embodiedP1(chain)
	maxHT := 24
	if htProduct != "" && !req.AllowBuy {
		most := 0.0
		for _, cl := range classes {
			for _, col := range cl.cols {
				g := 0.0
				for p, q := range col.out {
					for _, e := range emb[p] {
						g += q * e
					}
				}
				most = math.Max(most, g)
			}
		}
		per := 0.0
		for _, e := range emb[htProduct] {
			per += e
		}
		s := Schematics[htProduct]
		if per > 0 {
			rate := float64(slots) * most / per
			maxHT = min(maxHT, max(1, int(math.Ceil(rate/(float64(s.OutputQty)*3600/float64(s.CycleSec))-1e-9))))
		}
	}
	for _, cl := range classes {
		if cl.fixed {
			continue
		}
		if advProduct != "" || htProduct != "" {
			cl.cols = append(cl.cols, req.factoryColumns(cl, advProduct, htProduct, maxHT)...)
		}
		if cl.existing != nil {
			for i := range cl.cols {
				if !cl.cols[i].keep {
					cl.cols[i].edits = editsBetween(cl.existing, cl.cols[i].spec)
				}
			}
		}
	}
	pruneColumns(classes, free)

	// --- the integer program ---
	m := newLPModel()
	var zs []zref
	const perColonyFresh = 1e9 // target mode: fewest colonies first
	maxMode := req.PerDay <= 0
	colonyCost := perColonyFresh
	if maxMode {
		colonyCost = 50 // fewer colonies only when nothing else differs
	}
	editCost := 0.0
	if req.Adapt {
		editCost = colonyCost / 20
		if maxMode {
			editCost = 2000 // ISK per hour per structure changed: a few rebuilds are worth a real gain only
		}
	}
	// In target mode the colony count (with a twentieth of a colony
	// per structure edited) is minimised first, on its own, and ISK
	// second among the plans that reach that count. primary holds
	// the first objective.
	primary := map[int]float64{}
	for ci, cl := range classes {
		row := map[int]float64{}
		for j, col := range cl.cols {
			cost := colonyCost + col.edits*editCost
			if col.keep {
				cost = colonyCost
			}
			if !maxMode {
				primary[len(m.Obj)] = cost / perColonyFresh
				cost = 0
			}
			v := m.col(cost, 0, float64(cl.count), true)
			m.Pri[v] = 2
			zs = append(zs, zref{ci, j, v})
			row[v] = 1
		}
		m.row(row, senseLE, float64(cl.count))
		if cl.existing != nil {
			// The colony stands either way: it holds the slot, and
			// leaving it unused is a tear-down.
			m.row(row, senseEQ, 1)
		}
	}
	allZ := map[int]float64{}
	for _, z := range zs {
		allZ[z.v] = 1
	}
	m.row(allZ, senseLE, float64(slots))

	// Factory assignment: a[s] advanced/high-tech factories on schematic s.
	aVar, runVar := map[string]int{}, map[string]int{}
	advCap, htCap := map[int]float64{}, map[int]float64{}
	// No schematic can have more factories than every slot's best
	// factory colony holds.
	advMax, htMax := 0, 0
	for _, cl := range classes {
		for _, col := range cl.cols {
			advMax, htMax = max(advMax, col.adv), max(htMax, col.ht)
		}
	}
	for _, c := range chain {
		s, ok := Schematics[c]
		if !ok || s.Facility == FacilityBasic {
			continue
		}
		taxIn := 0.0
		for in, per := range s.Inputs {
			taxIn += float64(per) * 3600 / float64(s.CycleSec) * AllCommodities[in].TaxValue * req.TaxRate * 1.5
		}
		// Built count (whole) and how much of it runs (a factory
		// short of inputs idles part of the time).
		most := float64(slots * advMax)
		if s.Facility == FacilityHighTech {
			most = float64(slots * htMax)
		}
		v := m.col(0.01, 0, most, true)
		m.Pri[v] = 1
		m.Probe[v] = true
		run := m.col(taxIn, 0, -1, false)
		aVar[c] = v
		runVar[c] = run
		m.row(map[int]float64{run: 1, v: -1}, senseLE, 0)
		if s.Facility == FacilityAdvanced {
			advCap[v] = 1
		} else {
			htCap[v] = 1
		}
	}
	for _, z := range zs {
		col := classes[z.class].cols[z.col]
		if col.adv > 0 {
			advCap[z.v] = -float64(col.adv)
		}
		if col.ht > 0 {
			htCap[z.v] = -float64(col.ht)
		}
	}
	m.row(advCap, senseLE, 0)
	m.row(htCap, senseLE, 0)
	// Keep columns' pinned factories run at most their count.
	var keepRuns []keepRun
	for _, z := range zs {
		col := classes[z.class].cols[z.col]
		for _, prod := range sortedKeys(col.fixed) {
			n := col.fixed[prod]
			if _, ok := Schematics[prod]; !ok || !inChain[prod] {
				continue
			}
			s := Schematics[prod]
			taxIn := 0.0
			for in, per := range s.Inputs {
				taxIn += float64(per) * 3600 / float64(s.CycleSec) * AllCommodities[in].TaxValue * req.TaxRate * 1.5
			}
			v := m.col(taxIn, 0, -1, false)
			m.row(map[int]float64{v: 1, z.v: -float64(n)}, senseLE, 0)
			keepRuns = append(keepRuns, keepRun{v, z.v, prod, n})
		}
	}

	// Market.
	sellVar, buyVar := map[string]int{}, map[string]int{}
	for _, c := range chain {
		if TierOf(c) == TierRaw {
			continue
		}
		price, known := req.Prices[c]
		tax := AllCommodities[c].TaxValue * req.TaxRate
		if c == req.Target {
			gain := 0.0
			if known && price > 0 {
				gain = price - tax
			} else if maxMode {
				gain = 1 // no price: still make as much as possible
			}
			sellVar[c] = m.col(-gain, 0, -1, false)
		} else if req.SellSurplus && known && price > 0 {
			sellVar[c] = m.col(-(price - tax), 0, -1, false)
		}
		if req.AllowBuy && c != req.Target && known && price > 0 {
			buyVar[c] = m.col(price+tax*0.5, 0, -1, false)
		}
	}
	// Balance: made + bought >= used + sold.
	for _, c := range chain {
		if TierOf(c) == TierRaw {
			continue
		}
		row := map[int]float64{}
		for _, z := range zs {
			if q := classes[z.class].cols[z.col].out[c]; q > 0 {
				row[z.v] += q
			}
		}
		for prod, v := range runVar {
			s := Schematics[prod]
			if prod == c {
				row[v] += float64(s.OutputQty) * 3600 / float64(s.CycleSec)
			}
			if per, ok := s.Inputs[c]; ok {
				row[v] -= float64(per) * 3600 / float64(s.CycleSec)
			}
		}
		for _, k := range keepRuns {
			s := Schematics[k.product]
			if k.product == c {
				row[k.v] += float64(s.OutputQty) * 3600 / float64(s.CycleSec)
			}
			if per, ok := s.Inputs[c]; ok {
				row[k.v] -= float64(per) * 3600 / float64(s.CycleSec)
			}
		}
		if v, ok := buyVar[c]; ok {
			row[v] += 1
		}
		if v, ok := sellVar[c]; ok {
			row[v] -= 1
		}
		m.row(row, senseGE, 0)
	}
	if !maxMode {
		m.row(map[int]float64{sellVar[req.Target]: 1}, senseGE, req.PerDay/24)
	}

	// Counting columns: whole totals the relaxation would otherwise
	// spread across fractional colonies. The search splits on them
	// before single designs and narrows their ranges at the root.
	addCount := func(coef map[int]float64, most float64) {
		if len(coef) == 0 {
			return
		}
		v := m.col(0, 0, most, true)
		m.Pri[v] = 3
		m.Probe[v] = true
		coef[v] = -1
		m.row(coef, senseEQ, 0)
	}
	// Colonies in all, and factory colonies.
	facZ := map[int]float64{}
	for _, z := range zs {
		if col := classes[z.class].cols[z.col]; col.adv+col.ht > 0 {
			facZ[z.v] = 1
		}
	}
	addCount(copyCoef(allZ), float64(slots))
	addCount(facZ, float64(slots))
	// Extractor loads per P1: what each design ships of it, counting
	// the P1 already built into P2 on the planet, in units of the
	// commonest load.
	for _, c := range chain {
		if TierOf(c) != TierP1 {
			continue
		}
		load := map[int]float64{}
		for _, z := range zs {
			g := 0.0
			for p, q := range classes[z.class].cols[z.col].out {
				g += q * emb[p][c]
			}
			if g > 1e-9 {
				load[z.v] = g
			}
		}
		unit := loadUnit(load)
		coef, most := map[int]float64{}, 0.0
		for v, g := range load {
			coef[v] = math.Ceil(g/unit - 1e-9)
			most = math.Max(most, coef[v])
		}
		addCount(coef, most*float64(slots))
	}
	// Factories per product that extraction colonies or kept colonies
	// also make: those on such colonies plus those assigned.
	for _, c := range chain {
		s, ok := Schematics[c]
		if !ok || s.Facility == FacilityBasic {
			continue
		}
		rate := float64(s.OutputQty) * 3600 / float64(s.CycleSec)
		coef, most := map[int]float64{}, 0.0
		for _, z := range zs {
			if q := classes[z.class].cols[z.col].out[c]; q > 1e-9 {
				coef[z.v] = math.Ceil(q/rate - 1e-9)
				most = math.Max(most, coef[z.v])
			}
		}
		for _, k := range keepRuns {
			if k.product == c {
				coef[k.z] += float64(k.n)
				most = math.Max(most, coef[k.z])
			}
		}
		if len(coef) == 0 {
			continue
		}
		coef[aVar[c]] = 1
		addCount(coef, most*float64(slots)+m.Hi[aVar[c]])
	}

	const budget = 1200 * time.Millisecond
	started := time.Now()
	opt := milpOptions{budget: budget, maxNodes: 200000}
	var res milpResult
	exact := true
	feasible := true
	if !maxMode {
		// First the fewest colonies, then the best plan with that many.
		first := *m
		first.Obj = make([]float64, len(m.Obj))
		limit := map[int]float64{}
		for v, w := range primary {
			first.Obj[v] = w
			limit[v] = w
		}
		r1 := solveMILP(&first, milpOptions{budget: budget / 2, maxNodes: opt.maxNodes})
		if r1.ok {
			exact = r1.exact
			opt.start = r1.x
			m.row(limit, senseLE, r1.obj+1e-6)
		} else {
			feasible = false
		}
	}
	if feasible {
		opt.budget = budget - time.Since(started)
		res = solveMILP(m, opt)
		exact = exact && res.exact
	}
	x, ok := res.x, res.ok
	if ok {
		// A factory built but never run is not built.
		for c, v := range aVar {
			x[v] = math.Min(x[v], math.Max(0, math.Ceil(x[runVar[c]]-1e-6)))
		}
	}
	if !ok {
		short := "These planets and pilots cannot make that much."
		if maxMode {
			short = "These planets cannot supply this chain: a raw resource has no planet type in reach."
		}
		missing := []string{}
		for raw := range needRaw {
			found := false
			for _, cl := range classes {
				for _, y := range PlanetYields[cl.ptype] {
					if y == raw {
						found = true
					}
				}
			}
			if !found {
				missing = append(missing, raw)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 && !req.AllowBuy {
			short = "No planet in reach yields " + strings.Join(missing, ", ") + "."
		}
		return &Solution{Target: req.Target, Short: short, SlotsTotal: slots, ProgramHours: req.ProgramHours}, nil
	}

	sol := &Solution{Target: req.Target, SlotsTotal: slots, Exact: exact, ProgramHours: req.ProgramHours, PricesKnown: true}
	sol.PerDay = x[sellVar[req.Target]] * 24
	if sol.PerDay < 0.5 {
		sol.Short = fmt.Sprintf("%d planets cannot make even one %s a day from raw resources.", slots, req.Target)
		if !req.AllowBuy {
			sol.Short += " Allow buying inputs, or add pilots."
		}
	}
	for c, v := range sellVar {
		q := x[v] * 24
		if q < 1e-6 {
			continue
		}
		if p, ok := req.Prices[c]; ok && p > 0 {
			sol.RevenueDay += q * p
		} else {
			sol.PricesKnown = false
		}
		sol.TaxDay += q * AllCommodities[c].TaxValue * req.TaxRate
	}
	for c, v := range buyVar {
		q := x[v] * 24
		if q > 1e-6 {
			sol.BuyDay += q * req.Prices[c]
			sol.TaxDay += q * AllCommodities[c].TaxValue * req.TaxRate * 0.5
		}
	}

	realize(sol, req, classes, zs, keepRuns, aVar, runVar, buyVar, sellVar, x)
	sol.ProfitDay = sol.RevenueDay - sol.BuyDay - sol.TaxDay
	if !exact {
		sol.Notes = append(sol.Notes, "The search stopped at its time limit; this plan is good, not proven best.")
	}
	return sol, nil
}

type zref struct{ class, col, v int }

func copyCoef(m map[int]float64) map[int]float64 {
	out := make(map[int]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// embodiedP1 gives, for each product of the chain, the units of each
// P1 that go into one unit of it.
func embodiedP1(chain []string) map[string]map[string]float64 {
	emb := map[string]map[string]float64{}
	for _, p := range chain { // ordered raw first, target last
		switch TierOf(p) {
		case TierRaw:
		case TierP1:
			emb[p] = map[string]float64{p: 1}
		default:
			s := Schematics[p]
			e := map[string]float64{}
			for in, per := range s.Inputs {
				for c, q := range emb[in] {
					e[c] += q * float64(per) / float64(s.OutputQty)
				}
			}
			emb[p] = e
		}
	}
	return emb
}

// loadUnit picks the load size that the given loads are closest to
// whole multiples of, rounding each up.
func loadUnit(load map[int]float64) float64 {
	var sizes []float64
	for _, g := range load {
		sizes = append(sizes, g)
	}
	sort.Float64s(sizes)
	best, bestWaste := 1.0, math.Inf(1)
	for i, u := range sizes {
		if i > 0 && u-sizes[i-1] < 1e-9 {
			continue
		}
		waste := 0.0
		for _, g := range sizes {
			waste += (math.Ceil(g/u-1e-9)*u - g) / g
		}
		if waste < bestWaste-1e-12 {
			best, bestWaste = u, waste
		}
	}
	return best
}

// pruneColumns drops every colony design that another design matches
// or beats in all it offers. The better design must be on the same
// planet class, or on a class with room for every free slot when
// both are fresh planets.
func pruneColumns(classes []*planetClass, slots int) {
	covers := func(a, b *column) bool {
		if a.adv < b.adv || a.ht < b.ht || a.edits > b.edits {
			return false
		}
		for p, q := range b.out {
			if a.out[p] < q-1e-9 {
				return false
			}
		}
		return true
	}
	drop := make([][]bool, len(classes))
	for ci, cl := range classes {
		drop[ci] = make([]bool, len(cl.cols))
		for j := range cl.cols {
			b := &cl.cols[j]
			if b.keep {
				continue
			}
		search:
			for ck, other := range classes {
				if ck != ci && (cl.existing != nil || other.existing != nil || other.count < slots) {
					continue
				}
				for k := range other.cols {
					a := &other.cols[k]
					if a.keep || (ck == ci && k == j) || !covers(a, b) {
						continue
					}
					// Equals: the earlier one stays.
					if !covers(b, a) || ck < ci || (ck == ci && k < j) {
						drop[ci][j] = true
						break search
					}
				}
			}
		}
	}
	for ci, cl := range classes {
		kept := cl.cols[:0]
		for j, c := range cl.cols {
			if !drop[ci][j] {
				kept = append(kept, c)
			}
		}
		cl.cols = kept
	}
}

type keepRun struct {
	v, z    int
	product string
	n       int // factories the kept colony has on product
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(s []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
