package buildplan

// ---------------------------------------------------------------------------
// Unit costing: the planner engine's formulae (MaterialForRuns-
// Factored, JobFee) run as a memoized bottom-up evaluation over
// the blueprint graph, instead of a per-plan tree expansion.
//
// Build expands a tree: a component shared by forty parents is
// expanded forty times, which is right for a plan a pilot reads
// and wrong for pricing every manufacturable type in the game
// (several thousand roots, most of the tree shared). UnitCoster
// evaluates each type once: a type's per-unit build cost is its
// blueprint's one-run material cost -- every material itself at
// its own build cost, recursively, down to types with no
// blueprint, which price at the caller-supplied leaf price --
// plus the run's installation fee, all divided by the units one
// run produces.
//
// Two deliberate approximations, both standard for build-cost
// pricing and invisible at the magnitudes involved:
//
//   - fractional runs: a component consumed in the hundreds is
//     costed per unit, not rounded up to whole production runs
//     of that component the way a single real job would be;
//   - the installation fee of a node whose direct materials do
//     not all carry an adjusted price is left out of that node's
//     cost rather than guessed (materials dominate; see JobFee
//     for what the fee is charged on).
//
// A cycle in the graph (A builds from B builds from A) prices
// the repeated type at its leaf price, the same leaf the tree
// expansion falls back to.
// ---------------------------------------------------------------------------

// CostProfile is the facility and research state a unit costing
// assumes. ME is per-blueprint: ComponentME applies at every
// blueprint below the root, RootME at the root product itself
// (a caller pricing a ship hull passes its hull ME; a caller
// pricing anything else passes ComponentME for both). MatFactor
// and CostFactor are the facility multipliers Build takes
// (structure role bonus x rig bonus); CostIndex is the build
// system's manufacturing index, 0 = unknown (fees then price at
// zero contribution, never invented).
type CostProfile struct {
	RootME      int
	ComponentME int
	MatFactor   float64
	CostFactor  float64
	CostIndex   float64
	FacilityTax float64
}

// UnitCoster computes per-unit build costs over one recipe
// source with shared memoization. Not safe for concurrent use;
// one coster serves one pricing pass.
type UnitCoster struct {
	src       Source
	leafPrice func(typeID int64) (float64, bool)
	adjusted  map[int64]float64
	profile   CostProfile
	memo      map[int64]unitCostResult
	active    map[int64]bool
}

type unitCostResult struct {
	cost float64
	ok   bool
}

// NewUnitCoster wires a coster: src supplies blueprints by
// product, leafPrice prices a type that has no blueprint (or
// closes a cycle), adjusted carries the EIV base per type for
// installation fees (may be nil: fees then contribute nothing).
func NewUnitCoster(src Source, leafPrice func(typeID int64) (float64, bool), adjusted map[int64]float64, profile CostProfile) *UnitCoster {
	if profile.MatFactor <= 0 {
		profile.MatFactor = 1
	}
	if profile.CostFactor <= 0 {
		profile.CostFactor = 1
	}
	return &UnitCoster{
		src:       src,
		leafPrice: leafPrice,
		adjusted:  adjusted,
		profile:   profile,
		memo:      make(map[int64]unitCostResult),
		active:    make(map[int64]bool),
	}
}

// UnitCost is the per-unit build cost of a type treated as a
// component: its own blueprint runs at ComponentME. ok=false
// when some base input under it has no price at all -- the type
// then has no honest build cost.
func (c *UnitCoster) UnitCost(typeID int64) (float64, bool) {
	if r, hit := c.memo[typeID]; hit {
		return r.cost, r.ok
	}
	if c.active[typeID] {
		// A cycle: the repeated type prices as a buy leaf,
		// exactly where Build's expansion stops too. Not
		// memoized -- the value depends on the path in.
		return c.leafPrice(typeID)
	}
	bp, has := c.src.BlueprintForProduct(typeID)
	if !has || bp == nil {
		cost, ok := c.leafPrice(typeID)
		c.memo[typeID] = unitCostResult{cost, ok}
		return cost, ok
	}
	c.active[typeID] = true
	cost, ok := c.runCost(bp, c.profile.ComponentME)
	delete(c.active, typeID)
	c.memo[typeID] = unitCostResult{cost, ok}
	return cost, ok
}

// RootCost is the per-unit build cost of a type treated as the
// product being priced: its own blueprint runs at RootME, every
// blueprint beneath it at ComponentME (the memoized UnitCost).
// A type with no blueprint prices at its leaf price.
func (c *UnitCoster) RootCost(typeID int64) (float64, bool) {
	bp, has := c.src.BlueprintForProduct(typeID)
	if !has || bp == nil {
		return c.leafPrice(typeID)
	}
	if c.profile.RootME == c.profile.ComponentME {
		return c.UnitCost(typeID)
	}
	c.active[typeID] = true
	cost, ok := c.runCost(bp, c.profile.RootME)
	delete(c.active, typeID)
	return cost, ok
}

// runCost prices one run of bp at the given ME: discounted
// materials, each at its own recursive unit cost, plus the
// run's installation fee, divided by the run's output.
func (c *UnitCoster) runCost(bp *Blueprint, me int) (float64, bool) {
	var materials float64
	for _, m := range bp.Materials {
		need := MaterialForRunsFactored(m.Quantity, 1, me, c.profile.MatFactor)
		unit, ok := c.UnitCost(m.TypeID)
		if !ok {
			return 0, false
		}
		materials += float64(need) * unit
	}
	return (materials + c.jobFee(bp)) / float64(QuantityPerRun(bp)), true
}

// jobFee is one run's installation fee: the EIV (the blueprint's
// base quantities at adjusted price, ME0 and unbonused -- see
// JobFee) times the profile's index and facility terms. Zero
// when the index is unknown or any direct material lacks an
// adjusted price: an incomplete fee is left out, never padded.
func (c *UnitCoster) jobFee(bp *Blueprint) float64 {
	if c.profile.CostIndex <= 0 {
		return 0
	}
	var eiv float64
	for _, m := range bp.Materials {
		adj, ok := c.adjusted[m.TypeID]
		if !ok || adj <= 0 {
			return 0
		}
		eiv += float64(m.Quantity) * adj
	}
	return JobFee(eiv, c.profile.CostIndex, c.profile.CostFactor, c.profile.FacilityTax, false)
}
