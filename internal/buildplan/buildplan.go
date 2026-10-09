// Package buildplan is the industry build planner's engine: it expands
// a product into the tree of everything needed to build it, applies
// the EVE manufacturing formulae at each step, nets off what the
// pilot already holds, and prices what is left to buy. It is pure:
// the planner page supplies the recipes, blueprints, stock and
// prices, and it does no I/O of its own.
package buildplan

import (
	"errors"
	"sort"

	"evesynapse/internal/esi"
)

// ---------------------------------------------------------------------------
// Industry build planner: BOM expansion over the local
// SDE industry tables (schema 011), priced from the market guide
// cache and measured against the user's owned blueprints and
// stockpiles. The engine below is pure — it reads no database and
// makes no calls; the handler feeds it. Formulae follow the
// canonical EVE manufacturing model:
//
//   - material per node: ceil(baseQty × runs × (1 − ME/100)), with
//     the floor that a base-1 material never drops below one unit
//     per run (so its total is at least the run count);
//   - ME applies at the node whose blueprint consumes the
//     material; child output comes in whole runs, so a child's
//     runs are ceil(shortfall / units-per-run) and the excess is
//     surplus, never fractional runs;
//   - job time: ceil(baseSeconds × runs × (1 − TE/100)).
// ---------------------------------------------------------------------------

// nodeCap bounds one expansion; past it the plan reports
// "too large" instead of grinding.
const nodeCap = 2000

// maxDepth stops absurd nesting even below the node cap
// (real EVE chains are a handful of levels deep).
const maxDepth = 32

// ErrNotManufacturable marks a root product with no
// manufacturing blueprint in the local SDE.
var ErrNotManufacturable = errors.New("product has no manufacturing blueprint")

// Blueprint is one manufacturable product's recipe: the
// schema-011 blueprint row plus its materials and required skills.
type Blueprint struct {
	BlueprintTypeID    int64
	ProductTypeID      int64
	ProductQuantity    int64 // units produced per run, >= 1
	MaxProductionLimit int64
	TimeSeconds        int64 // base seconds per run, before TE
	Materials          []Material
	Skills             []SkillReq
}

// Material is one per-run input of a blueprint.
type Material struct {
	TypeID   int64
	Quantity int64 // base units per run, before ME
}

// SkillReq is one manufacturing skill requirement.
type SkillReq struct {
	TypeID int64
	Level  int64
}

// Source supplies blueprints by product type. The handler's
// source reads the SDE tables with a per-render memo; tests serve
// fixtures from a map.
type Source interface {
	BlueprintForProduct(productTypeID int64) (*Blueprint, bool)
}

// OwnedBlueprint is the best owned copy of one blueprint type: ME
// and TE percentages as ESI reports them (ME 0-10, TE 0-20).
type OwnedBlueprint struct {
	ME int
	TE int
}

// Input is everything Build needs besides the recipe
// source. Stock is the aggregate on-hand per type across the
// user's characters; Build copies it before allocating (the
// caller's map is never mutated).
type Input struct {
	Runs       int64
	MEOverride map[int64]int // blueprint type ID → ME 0-10
	Owned      map[int64]OwnedBlueprint
	Stock      map[int64]int64
	Prices     map[int64]esi.MarketPrice // market-guide cache; nil = no prices at all
}

// Node is one node of the expansion tree: an item the plan
// needs, either built (Blueprint set) or bought (a leaf).
type Node struct {
	TypeID      int64
	Depth       int
	BasePerRun  int64 // per-run qty in the consuming blueprint (0 for the root)
	RequiredQty int64 // units the plan needs here (root: the target output)
	Have        int64 // allocated from stock (0 for the root)
	Shortfall   int64 // RequiredQty − Have (leaf: to buy; build: to produce)

	Blueprint   *Blueprint // nil → buy leaf
	Runs        int64      // build runs (build nodes)
	ProducedQty int64      // Runs × units per run (build nodes)
	SurplusQty  int64      // ProducedQty − Shortfall
	ME          int
	TE          int
	MESource    string // "override" | "owned" | "assumed"
	TimeSeconds int64  // build nodes: total job seconds for its runs
	Cycle       bool   // this item already appears above it: shown as a buy leaf
	DepthCapped bool   // expansion stopped at the depth cap

	UnitPrice    float64
	PriceKnown   bool
	LineCost     float64 // leaf: Shortfall × unit price; build: rolled-up buy cost beneath
	CostComplete bool    // false when some price beneath is unknown

	// Per-node buy-vs-build. BuildCost
	// is the recursive build cost (materials + job costs);
	// BuyCost is Shortfall × unit price; BvBDelta = BuyCost −
	// BuildCost (positive = building is cheaper). BvBApplicable
	// is false for blueprint-type products (ESI lists BPO prices,
	// not BPC contract prices).
	BuildCost     float64
	BuyCost       float64
	BvBDelta      float64
	BvBApplicable bool

	Children []*Node
}

// ShopLine is one aggregated shopping-list entry: a leaf type
// the plan still has to buy, summed across the whole tree.
type ShopLine struct {
	TypeID     int64
	Quantity   int64
	UnitPrice  float64
	PriceKnown bool
	LineCost   float64
}

// Result is a finished expansion plus its totals.
type Result struct {
	Root          *Node
	TooLarge      bool
	TotalSeconds  int64 // sum of every build node's job time (sequential)
	Shopping      []ShopLine
	UnpricedLines int // shopping lines with no price data
}

// MaterialForRuns is the EVE material formula: the runs-adjusted,
// ME-discounted quantity for one material of one blueprint,
// integers throughout (ceil via +99). The base-1 floor keeps a
// 1-per-run material from ever discounting below one per run.
func MaterialForRuns(baseQty, runs int64, me int) int64 {
	if baseQty <= 0 || runs <= 0 {
		return 0
	}
	if me < 0 {
		me = 0
	}
	if me > 10 {
		me = 10
	}
	need := (baseQty*runs*int64(100-me) + 99) / 100
	if baseQty == 1 && need < runs {
		need = runs
	}
	return need
}

// JobSeconds is the EVE job-time formula for one build node.
func JobSeconds(baseSeconds, runs int64, te int) int64 {
	if baseSeconds <= 0 || runs <= 0 {
		return 0
	}
	if te < 0 {
		te = 0
	}
	if te > 20 {
		te = 20
	}
	return (baseSeconds*runs*int64(100-te) + 99) / 100
}

// UnitPrice picks the market-guide price for one type: the average
// price when usable, the adjusted price behind it. ok=false when
// the cache has nothing usable — callers say "no price data" and
// never price at an invented zero.
func UnitPrice(prices map[int64]esi.MarketPrice, typeID int64) (price float64, ok bool) {
	p, hit := prices[typeID]
	if !hit {
		return 0, false
	}
	if p.AveragePrice > 0 {
		return p.AveragePrice, true
	}
	if p.AdjustedPrice > 0 {
		return p.AdjustedPrice, true
	}
	return 0, false
}

// Build expands the recipe for rootProductTypeID into a full
// plan. Allocation walks the tree pre-order (a node's own stock is
// claimed before its children are planned, materials in type-ID
// order), so a shared stockpile is never promised twice.
func Build(src Source, rootProductTypeID int64, in Input) (*Result, error) {
	rootBP, ok := src.BlueprintForProduct(rootProductTypeID)
	if !ok || rootBP == nil {
		return nil, ErrNotManufacturable
	}
	runs := in.Runs
	if runs < 1 {
		runs = 1
	}
	b := &builder{
		src:       src,
		overrides: in.MEOverride,
		owned:     in.Owned,
		stock:     make(map[int64]int64, len(in.Stock)),
		prices:    in.Prices,
	}
	for id, qty := range in.Stock {
		if qty > 0 {
			b.stock[id] = qty
		}
	}

	res := &Result{}
	root := b.expand(rootProductTypeID, rootBP, 0, 0, runs*QuantityPerRun(rootBP), true, nil)
	res.Root = root
	if b.tooLarge {
		res.TooLarge = true
		return res, nil
	}

	// Totals + shopping aggregator: one walk collects job time
	// and the per-type buy sums.
	shop := make(map[int64]*ShopLine)
	res.TotalSeconds = 0
	Walk(root, func(n *Node) {
		res.TotalSeconds += n.TimeSeconds
		if n.Blueprint == nil && n.Shortfall > 0 {
			line, ok := shop[n.TypeID]
			if !ok {
				line = &ShopLine{TypeID: n.TypeID, PriceKnown: true}
				shop[n.TypeID] = line
			}
			line.Quantity += n.Shortfall
		}
	})
	for _, line := range shop {
		if p, ok := UnitPrice(in.Prices, line.TypeID); ok {
			line.UnitPrice = p
			line.LineCost = p * float64(line.Quantity)
		} else {
			line.PriceKnown = false
			res.UnpricedLines++
		}
		res.Shopping = append(res.Shopping, *line)
	}
	sort.Slice(res.Shopping, func(i, j int) bool { return res.Shopping[i].TypeID < res.Shopping[j].TypeID })
	return res, nil
}

// Walk visits every node pre-order.
func Walk(n *Node, fn func(*Node)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		Walk(c, fn)
	}
}

// QuantityPerRun is a blueprint's units per run, defensive minimum 1.
func QuantityPerRun(bp *Blueprint) int64 {
	if bp == nil || bp.ProductQuantity < 1 {
		return 1
	}
	return bp.ProductQuantity
}

// builder carries one expansion's mutable state.
type builder struct {
	src       Source
	overrides map[int64]int
	owned     map[int64]OwnedBlueprint
	stock     map[int64]int64 // remaining on-hand per type
	prices    map[int64]esi.MarketPrice
	nodes     int
	tooLarge  bool
}

// resolveME picks a node's ME/TE: an explicit override wins on ME,
// an owned copy supplies both otherwise, and the fallback is an
// unresearched blueprint (ME 0 / TE 0, "assumed"). An override over
// an owned copy keeps the copy's TE — it is the same physical
// blueprint, only hand-tuned.
func (b *builder) resolveME(bp *Blueprint) (me, te int, source string) {
	source = "assumed"
	if ob, ok := b.owned[bp.BlueprintTypeID]; ok {
		me, te, source = ob.ME, ob.TE, "owned"
	}
	if ov, ok := b.overrides[bp.BlueprintTypeID]; ok {
		me, source = ov, "override"
	}
	if me < 0 {
		me = 0
	}
	if me > 10 {
		me = 10
	}
	if te < 0 {
		te = 0
	}
	if te > 20 {
		te = 20
	}
	return me, te, source
}

// expand plans one node: typeID is needed in requiredQty units.
// bp is the type's blueprint when already resolved by the caller
// (nil = resolve here); path holds the ancestor product types for
// the cycle guard. Stock is claimed before children are planned.
func (b *builder) expand(typeID int64, bp *Blueprint, depth int, basePerRun, requiredQty int64, isRoot bool, path map[int64]bool) *Node {
	node := &Node{
		TypeID:       typeID,
		Depth:        depth,
		BasePerRun:   basePerRun,
		RequiredQty:  requiredQty,
		CostComplete: true,
	}
	b.nodes++
	if b.nodes > nodeCap {
		b.tooLarge = true
		return node
	}

	if !isRoot {
		if have := b.stock[typeID]; have > 0 {
			if have > requiredQty {
				have = requiredQty
			}
			node.Have = have
			b.stock[typeID] -= have
		}
	}
	node.Shortfall = requiredQty - node.Have

	if bp == nil {
		resolved, ok := b.src.BlueprintForProduct(typeID)
		if !ok {
			bp = nil
		} else {
			bp = resolved
		}
	}

	// Leaf outcomes: no blueprint, fully covered by stock, a
	// cycle back to an ancestor, or the depth safety cap. A leaf
	// prices its shortfall at the market guide.
	leaf := func() *Node {
		if p, ok := UnitPrice(b.prices, typeID); ok {
			node.UnitPrice = p
			node.PriceKnown = true
			node.LineCost = p * float64(node.Shortfall)
			node.BuyCost = node.LineCost
			// Leaves can't be built; BvB not applicable.
			// The blueprint guard: if this leaf IS a
			// blueprint type, market prices are BPO not BPC — BvB
			// would be meaningless, so mark inapplicable.
			node.BvBApplicable = false
		} else {
			node.CostComplete = false
		}
		return node
	}
	switch {
	case node.Shortfall <= 0:
		node.CostComplete = true
		return node
	case bp == nil:
		return leaf()
	case path[typeID]:
		node.Cycle = true
		return leaf()
	case depth >= maxDepth:
		node.DepthCapped = true
		return leaf()
	}

	// Build node.
	node.Blueprint = bp
	node.ME, node.TE, node.MESource = b.resolveME(bp)
	perRun := QuantityPerRun(bp)
	node.Runs = (node.Shortfall + perRun - 1) / perRun
	node.ProducedQty = node.Runs * perRun
	node.SurplusQty = node.ProducedQty - node.Shortfall
	node.TimeSeconds = JobSeconds(bp.TimeSeconds, node.Runs, node.TE)

	childPath := path
	if depth == 0 || path == nil {
		childPath = make(map[int64]bool, len(path)+1)
		for id := range path {
			childPath[id] = true
		}
	}
	childPath[typeID] = true

	mats := append([]Material(nil), bp.Materials...)
	sort.Slice(mats, func(i, j int) bool { return mats[i].TypeID < mats[j].TypeID })
	for _, m := range mats {
		need := MaterialForRuns(m.Quantity, node.Runs, node.ME)
		child := b.expand(m.TypeID, nil, depth+1, m.Quantity, need, false, childPath)
		node.Children = append(node.Children, child)
		node.LineCost += child.LineCost
		if !child.CostComplete {
			node.CostComplete = false
		}
		if b.tooLarge {
			break
		}
	}
	// Per-node buy-vs-build. BuildCost is the rolled-up
	// material cost (LineCost); BuyCost is market price for the
	// shortfall. Delta positive = building saves money.
	node.BuildCost = node.LineCost
	if p, ok := UnitPrice(b.prices, typeID); ok {
		node.UnitPrice = p
		node.PriceKnown = true
		node.BuyCost = p * float64(node.Shortfall)
		node.BvBDelta = node.BuyCost - node.BuildCost
		// The blueprint guard: blueprint-type products are excluded
		// from BvB (ESI lists BPO prices, not BPC contract prices).
		// A product with a blueprint is not itself a blueprint, so
		// BvB applies here; the guard matters for leaf materials
		// that ARE blueprint types (handled in leaf()).
		node.BvBApplicable = true
	}
	return node
}
