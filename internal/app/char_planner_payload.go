package app

// Planner payload: one JSON blob per computed plan carrying the
// full build tree with every figure the client may display. The
// client duplicates no formula: it only sums server figures over
// make/buy masks (toggles and presets). Each preset's canonical
// totals ride along; the client compares in integer cents and
// falls back to the server figures on any mismatch, so a client
// bug shows server truth, never an invented number.

import (
	"context"
	"encoding/json"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/logging"
)

const (
	// plannerForgeRegionID is The Forge: the Jita sell source.
	plannerForgeRegionID = 10000002
	// plannerDefaultSystemID is Jita: the default build system
	// for the cost-index lookup (the operator overrides it).
	plannerDefaultSystemID = 30000142
	// plannerCostActivity is the ESI industry cost-index
	// activity for manufacturing (the activity names in
	// industry_cost_indices are ESI's, not SDE IDs).
	plannerCostActivity = "manufacturing"
	// plannerMarketJita prices from Forge sell rows;
	// plannerMarketGuide from the cluster price guide.
	plannerMarketJita  = "jita"
	plannerMarketGuide = "guide"
)

// planCents rounds ISK to integer cents: the unit every payload
// money field uses, so client and server compare exactly.
func planCents(f float64) int64 {
	return int64(math.Round(f * 100))
}

// planNodeDTO is one engine node, JSON-ready. Money is integer
// cents, durations whole seconds; Kids nest the subtree.
type planNodeDTO struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	Depth     int            `json:"depth"`
	Build     bool           `json:"build"`
	Need      int64          `json:"need"`
	Have      int64          `json:"have"`
	Short     int64          `json:"short"`
	Runs      int64          `json:"runs"`
	Produced  int64          `json:"produced"`
	Surplus   int64          `json:"surplus"`
	ME        int            `json:"me"`
	TE        int            `json:"te"`
	MESource  string         `json:"meSrc"`
	TimeSec   int64          `json:"timeSec"`
	Materials int64          `json:"materials"`
	Complete  bool           `json:"complete"`
	Unit      int64          `json:"unit"`
	Known     bool           `json:"known"`
	Buy       int64          `json:"buy"`
	Job       int64          `json:"job"`
	JobKnown  bool           `json:"jobKnown"`
	Save      int64          `json:"save"`
	SaveKnown bool           `json:"saveKnown"`
	Delta     int64          `json:"delta"`
	DeltaOn   bool           `json:"deltaOn"`
	Cycle     bool           `json:"cycle"`
	Capped    bool           `json:"capped"`
	Kids      []*planNodeDTO `json:"kids,omitempty"`
}

// planNodeToDTO converts one engine node. factor scales job
// time (trained skills, manual override); money rounds to cents.
func planNodeToDTO(n *buildplan.Node, nameOf func(int64) string, factor float64) *planNodeDTO {
	d := &planNodeDTO{
		ID: n.TypeID, Name: nameOf(n.TypeID),
		URL:   "/items/type/" + strconv.FormatInt(n.TypeID, 10) + "/",
		Depth: n.Depth, Build: n.Blueprint != nil,
		Need: n.RequiredQty, Have: n.Have, Short: n.Shortfall,
		Runs: n.Runs, Produced: n.ProducedQty, Surplus: n.SurplusQty,
		ME: n.ME, TE: n.TE, MESource: n.MESource,
		TimeSec:   int64(math.Round(float64(n.TimeSeconds) * factor)),
		Materials: planCents(n.LineCost), Complete: n.CostComplete,
		Unit: planCents(n.UnitPrice), Known: n.PriceKnown,
		Buy: planCents(n.BuyCost),
		Job: planCents(n.JobCost), JobKnown: n.JobKnown,
		Save: planCents(n.ResearchSaving), SaveKnown: n.ResearchKnown,
		Delta: planCents(n.BvBDelta), DeltaOn: n.BvBApplicable,
		Cycle: n.Cycle, Capped: n.DepthCapped,
	}
	for _, c := range n.Children {
		d.Kids = append(d.Kids, planNodeToDTO(c, nameOf, factor))
	}
	return d
}

// planPreset is one scenario mask over the tree: full builds
// every buildable node, optimized builds where building saves
// (or ties) and buys the rest, buyComponents builds only the
// root. The client mirrors these masks exactly; the server's
// totals are the canonical cross-check.
type planPreset int

const (
	planPresetFull planPreset = iota
	planPresetOptimized
	planPresetBuyComponents
)

// planShopLine is one aggregated buy line under a preset.
// UnitLine/CostLine are display strings ("no price data" when
// unknown) so the static list needs no client math.
type planShopLine struct {
	TypeID   int64  `json:"id"`
	Name     string `json:"name"`
	Qty      int64  `json:"qty"`
	Unit     int64  `json:"unit"`
	Cost     int64  `json:"cost"`
	Known    bool   `json:"known"`
	UnitLine string `json:"unitLine"`
	CostLine string `json:"costLine"`
}

// planJobSpan is one scheduled build job for the Gantt view:
// server-computed start/end/slot/critical, rendered as-is.
type planJobSpan struct {
	TypeID   int64  `json:"id"`
	Name     string `json:"name"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Slot     int    `json:"slot"`
	Critical bool   `json:"critical"`
}

// planPresetTotals is the canonical economics of one preset.
// Money is integer cents; ElapsedSeconds is the slot-aware
// schedule makespan. Incomplete counts nodes whose shortfall
// has no price; FeesKnown is false while any built job's fee
// is unknown.
type planPresetTotals struct {
	Materials int64 `json:"materials"`
	Fees      int64 `json:"fees"`
	FeesKnown bool  `json:"feesKnown"`
	Landed    int64 `json:"landed"`
	// Bought is the at-market cost of purchased lines; Built
	// the market cost of raw materials consumed by built jobs.
	// Bought + Built always equals Materials.
	Bought    int64          `json:"bought"`
	Built     int64          `json:"built"`
	Complete  bool           `json:"complete"`
	Unpriced  int            `json:"unpriced"`
	Value     int64          `json:"value"`
	Known     bool           `json:"valueKnown"`
	Profit    int64          `json:"profit"`
	ProfKnown bool           `json:"profKnown"`
	MarginPct float64        `json:"marginPct"`
	Elapsed   int64          `json:"elapsed"`
	BuyValue  int64          `json:"buyValue"`
	StockVal  int64          `json:"stockVal"`
	Shopping  []planShopLine `json:"shopping"`
	Jobs      []planJobSpan  `json:"jobs"`
}

// planRollup accumulates one preset's totals over the tree.
// Bought subtrees are pruned (their jobs, fees and shopping
// never accrue); covered nodes cost nothing.
type planRollup struct {
	preset planPreset
	factor float64
	tot    planPresetTotals
	shop   map[int64]int64
	jobs   []*planSchedNode
}

type planSchedNode struct {
	n    *buildplan.Node
	dur  int64
	deps []*planSchedNode
}

func (r *planRollup) buyNode(n *buildplan.Node, isRoot bool) bool {
	if n.Blueprint == nil {
		return true // leaves only ever buy
	}
	switch r.preset {
	case planPresetBuyComponents:
		return !isRoot
	case planPresetOptimized:
		return n.BvBApplicable && n.BvBDelta < 0 && n.PriceKnown
	default:
		return false
	}
}

func (r *planRollup) addLeaf(n *buildplan.Node, built bool) {
	if n.Shortfall <= 0 {
		return
	}
	r.shop[n.TypeID] += n.Shortfall
	if n.PriceKnown {
		c := planCents(n.LineCost)
		r.tot.Materials += c
		r.tot.BuyValue += c
		if built {
			r.tot.Built += c
		} else {
			r.tot.Bought += c
		}
	} else {
		r.tot.Complete = false
		r.tot.Unpriced++
	}
}

func (r *planRollup) add(n *buildplan.Node, isRoot, built bool) *planSchedNode {
	if n.Shortfall <= 0 {
		return nil // covered by stock: no cost, no time
	}
	if n.Blueprint == nil {
		r.addLeaf(n, built)
		return nil
	}
	if r.buyNode(n, isRoot) {
		// The finished product is never a shopping line: when
		// a scenario buys the root outright, its cost still
		// accrues below, but the buy verdict lives in the
		// header, not in the list of inputs to buy.
		if !isRoot {
			r.shop[n.TypeID] += n.Shortfall
		}
		if n.PriceKnown {
			c := planCents(n.BuyCost)
			r.tot.Materials += c
			r.tot.BuyValue += c
			r.tot.Bought += c
		} else {
			r.tot.Complete = false
			r.tot.Unpriced++
		}
		return nil // bought: the subtree is pruned
	}
	sn := &planSchedNode{n: n, dur: int64(math.Round(float64(n.TimeSeconds) * r.factor))}
	if n.JobKnown {
		r.tot.Fees += planCents(n.JobCost)
	} else {
		r.tot.FeesKnown = false
	}
	for _, c := range n.Children {
		if dep := r.add(c, false, true); dep != nil {
			sn.deps = append(sn.deps, dep)
		}
	}
	r.jobs = append(r.jobs, sn)
	return sn
}

// planSchedule runs deterministic list scheduling over the
// preset's jobs: post-order (children first), ties by type ID,
// each job on the lowest free slot once its dependencies end.
// It returns spans and the makespan.
func planSchedule(jobs []*planSchedNode, slots int, nameOf func(int64) string) ([]planJobSpan, int64) {
	if slots < 1 {
		slots = 1
	}
	ordered := append([]*planSchedNode(nil), jobs...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].n.Depth != ordered[j].n.Depth {
			return ordered[i].n.Depth > ordered[j].n.Depth
		}
		return ordered[i].n.TypeID < ordered[j].n.TypeID
	})
	free := make([]int64, slots)
	end := make(map[*planSchedNode]int64, len(ordered))
	at := make(map[*planSchedNode]int, len(ordered))
	spans := make([]planJobSpan, 0, len(ordered))
	var makespan int64
	for _, sn := range ordered {
		var ready int64
		for _, d := range sn.deps {
			if end[d] > ready {
				ready = end[d]
			}
		}
		slot := 0
		for s := 1; s < slots; s++ {
			if free[s] < free[slot] {
				slot = s
			}
		}
		start := ready
		if free[slot] > start {
			start = free[slot]
		}
		finish := start + sn.dur
		free[slot] = finish
		end[sn] = finish
		if finish > makespan {
			makespan = finish
		}
		at[sn] = len(spans)
		spans = append(spans, planJobSpan{
			TypeID: sn.n.TypeID, Name: nameOf(sn.n.TypeID),
			Start: start, End: finish, Slot: slot,
		})
	}
	// Critical path: longest duration chain ending at the root
	// job, walked back through built dependencies.
	longest := make(map[*planSchedNode]int64, len(ordered))
	var chain func(sn *planSchedNode) int64
	chain = func(sn *planSchedNode) int64 {
		if v, ok := longest[sn]; ok {
			return v
		}
		best := int64(0)
		for _, d := range sn.deps {
			if v := chain(d); v > best {
				best = v
			}
		}
		longest[sn] = sn.dur + best
		return longest[sn]
	}
	if len(ordered) > 0 {
		var root *planSchedNode
		for _, sn := range ordered {
			if sn.n.Depth == 0 {
				root = sn
				break
			}
		}
		if root == nil {
			root = ordered[0]
		}
		chain(root)
		for cur := root; cur != nil; {
			spans[at[cur]].Critical = true
			var next *planSchedNode
			var best int64 = -1
			for _, d := range cur.deps {
				if longest[d] > best {
					best, next = longest[d], d
				}
			}
			cur = next
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Start != spans[j].Start {
			return spans[i].Start < spans[j].Start
		}
		return spans[i].Slot < spans[j].Slot
	})
	return spans, makespan
}

// planPayload is the whole client blob: the tree with every
// figure, the three canonical preset totals (the cross-check),
// and the run's inputs. Money is integer cents throughout.
type planPayload struct {
	Product    int64            `json:"product"`
	Name       string           `json:"name"`
	Produced   int64            `json:"produced"`
	Market     string           `json:"market"`
	MarketName string           `json:"marketName"`
	SystemID   int64            `json:"system"`
	SystemName string           `json:"systemName"`
	IndexPct   float64          `json:"indexPct"`
	IndexKnown bool             `json:"indexKnown"`
	PricesNote string           `json:"pricesNote"`
	Slots      int              `json:"slots"`
	SlotsNote  string           `json:"slotsNote"`
	TimeNote   string           `json:"timeNote"`
	Root       *planNodeDTO     `json:"root"`
	Full       planPresetTotals `json:"full"`
	Optimized  planPresetTotals `json:"optimized"`
	BuyComp    planPresetTotals `json:"buyComp"`
	// StockVal is the priced stockpile behind the hub split.
	// It never depends on the mask, so it sits at top level.
	StockVal int64 `json:"stockVal"`
}

// planTimeSkills resolves the manufacturing time skills' type
// IDs from static data by name (IDs are CCP's to change).
func (app *Application) planTimeSkills(ctx context.Context) map[string]int64 {
	out := map[string]int64{"Industry": 0, "Advanced Industry": 0}
	rows, err := app.queries.ListSDETypesByNames(ctx, []string{"Industry", "Advanced Industry"})
	if err != nil {
		logging.Errorf("planner: time skill IDs: %v", err)
		return out
	}
	for _, r := range rows {
		if _, ok := out[r.Name]; ok {
			out[r.Name] = r.TypeID
		}
	}
	return out
}

// planTimeFactor is the job-time multiplier from the scope's
// trained Industry (4%/level) and Advanced Industry (3%/level),
// multiplicative after TE. Unknown skills (or no scope) mean
// no reduction; a manual ?timef= value wins when sane.
func planTimeFactor(raw string, ids map[string]int64, data plannerScopeData, chars []db.Character) (float64, string) {
	if f, err := strconv.ParseFloat(raw, 64); err == nil && f >= 0.05 && f <= 1 {
		return f, "Manual time factor."
	}
	best := func(skillID int64) int {
		level := 0
		for _, ch := range chars {
			if trained, ok := data.Skills[ch.CharacterID]; ok && trained[skillID] > level {
				level = trained[skillID]
			}
		}
		return level
	}
	ind, adv := 0, 0
	if id := ids["Industry"]; id > 0 {
		ind = best(id)
	}
	if id := ids["Advanced Industry"]; id > 0 {
		adv = best(id)
	}
	if ind == 0 && adv == 0 {
		return 1, "No time skills on file: full build times shown."
	}
	factor := (1 - 0.04*float64(ind)) * (1 - 0.03*float64(adv))
	return factor, "Industry and Advanced Industry applied from trained skills."
}

// planSlots counts manufacturing job slots: one plus Mass
// Production plus Advanced Mass Production, best across the
// scope; a manual ?slots= value wins when sane.
func planSlots(raw string, ids map[string]int64, data plannerScopeData, chars []db.Character) (int, string) {
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 20 {
		return n, "Manual slot count."
	}
	best := func(name string) int {
		level := 0
		for _, ch := range chars {
			if trained, ok := data.Skills[ch.CharacterID]; ok && trained[ids[name]] > level {
				level = trained[ids[name]]
			}
		}
		return level
	}
	slots := 1
	if ids["Mass Production"] > 0 {
		slots += best("Mass Production")
	}
	if ids["Advanced Mass Production"] > 0 {
		slots += best("Advanced Mass Production")
	}
	if slots <= 1 {
		return 1, "One slot: no Mass Production on file."
	}
	return slots, "Slots from trained Mass Production."
}

// planFinishPreset fills value/profit/shopping/schedule around
// a rollup: product sell value, margin, stock value, buy lines,
// and the slot-aware Gantt spans.
func planFinishPreset(r *planRollup, root *buildplan.Node, prices map[int64]esi.MarketPrice, slots int, nameOf func(int64) string, unitOf func(int64) (float64, bool)) {
	t := &r.tot
	t.Landed = t.Materials + t.Fees
	// Unit first, then multiply: the client sees the same
	// rounded unit, so both sides round identically.
	if p, ok := buildplan.UnitPrice(prices, root.TypeID); ok {
		unit := planCents(p)
		t.Value = unit * root.ProducedQty
		t.Known = true
		if t.Complete && t.FeesKnown && t.Value > 0 {
			t.Profit = t.Value - t.Landed
			t.ProfKnown = true
			t.MarginPct = float64(t.Profit) / float64(t.Value) * 100
		}
	}
	ids := make([]int64, 0, len(r.shop))
	for id := range r.shop {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return nameOf(ids[i]) < nameOf(ids[j]) })
	for _, id := range ids {
		line := planShopLine{TypeID: id, Name: nameOf(id), Qty: r.shop[id]}
		if p, ok := unitOf(id); ok {
			line.Unit = planCents(p)
			line.Cost = line.Unit * line.Qty
			line.Known = true
			line.UnitLine = iskCompactCents(line.Unit)
			line.CostLine = iskCompactCents(line.Cost)
		} else {
			line.UnitLine = "no price data"
		}
		t.Shopping = append(t.Shopping, line)
	}
	t.Jobs, t.Elapsed = planSchedule(r.jobs, slots, nameOf)
}

// iskCompactCents formats integer cents compactly for dense
// dashboards: 81.42B, 3.10M, 250.00, always with ISK. The
// client mirrors this exactly (plan.js iskC), so keep the two
// in step.
func iskCompactCents(c int64) string {
	neg := c < 0
	if neg {
		c = -c
	}
	f := float64(c) / 100
	var s string
	switch {
	case f >= 1e9:
		s = strconv.FormatFloat(f/1e9, 'f', 2, 64) + "B"
	case f >= 1e6:
		s = strconv.FormatFloat(f/1e6, 'f', 2, 64) + "M"
	case f >= 1e3:
		s = strconv.FormatFloat(f/1e3, 'f', 2, 64) + "K"
	default:
		s = strconv.FormatFloat(f, 'f', 2, 64)
	}
	if neg {
		s = "-" + s
	}
	return s + " ISK"
}

// planHeaderLines formats the optimized preset for the header
// stat blocks: landed cost, market value, margin, elapsed.
// Unknowns stay visible as unknowns, never zeros.
func planHeaderLines(p *planPayload) (landed, landedSub, value, margin, marginSub, elapsed, elapsedSub string) {
	b := p.Optimized
	landedSub = "materials"
	if b.FeesKnown && b.Fees > 0 {
		landedSub = "materials + fees"
	} else if !b.FeesKnown {
		landedSub = "materials (fees unknown)"
	}
	if b.Complete {
		landed = iskCompactCents(b.Landed)
	} else {
		landed = "partial"
		landedSub += " · some prices missing"
	}
	if b.Known {
		value = iskCompactCents(b.Value)
	} else {
		value = "—"
	}
	if b.ProfKnown {
		margin = iskCompactCents(b.Profit)
		if b.Profit > 0 {
			margin = "+" + margin
		}
		sub := ""
		if b.Value > 0 {
			sub = strconv.FormatFloat(b.MarginPct, 'f', 1, 64) + "% of market value"
		}
		if b.Elapsed > 0 {
			perHour := float64(b.Profit) / 100 / (float64(b.Elapsed) / 3600)
			if sub != "" {
				sub += " · "
			}
			sub += iskCompactCents(planCents(perHour)) + "/hour"
		}
		marginSub = sub
	} else {
		margin = "—"
		marginSub = "needs full prices"
	}
	elapsed = humanDuration(time.Duration(b.Elapsed) * time.Second)
	elapsedSub = "critical path · " + strconv.Itoa(p.Slots) + " slot"
	if p.Slots != 1 {
		elapsedSub += "s"
	}
	return landed, landedSub, value, margin, marginSub, elapsed, elapsedSub
}

// planBuyOutrightLine compares buying the finished product
// outright against the optimized build: the header's
// build-vs-buy verdict in one line.
func planBuyOutrightLine(p *planPayload) string {
	b := p.Optimized
	if !b.Known || !b.ProfKnown {
		return ""
	}
	if b.Profit >= 0 {
		return p.MarketName + " sell " + iskCompactCents(b.Value) + " — building saves " + iskCompactCents(b.Profit)
	}
	return p.MarketName + " sell " + iskCompactCents(b.Value) + " — buying outright saves " + iskCompactCents(-b.Profit)
}

// planStockValue prices the aggregate stockpile for the hub
// split: bought value comes from the preset, stock here.
func planStockValue(stock map[int64]int64, unitOf func(int64) (float64, bool)) int64 {
	var total int64
	for id, qty := range stock {
		if p, ok := unitOf(id); ok {
			total += planCents(p * float64(qty))
		}
	}
	return total
}

// buildPlanPayload computes the client blob around an expanded
// result: DTO tree, canonical preset totals with schedules,
// and the run's inputs. prices selects the market source; the
// unit function prices one type under it for shopping lines.
func buildPlanPayload(res *buildplan.Result, productID int64, prices map[int64]esi.MarketPrice, unitOf func(int64) (float64, bool), factor float64, slots int, stock map[int64]int64, nameOf func(int64) string) *planPayload {
	root := res.Root
	p := &planPayload{
		Product: productID, Name: nameOf(productID),
		Produced: root.ProducedQty,
		Root:     planNodeToDTO(root, nameOf, factor),
	}
	for _, preset := range []planPreset{planPresetFull, planPresetOptimized, planPresetBuyComponents} {
		r := &planRollup{preset: preset, factor: factor, shop: make(map[int64]int64)}
		r.tot.Complete = true
		r.tot.FeesKnown = true
		r.add(root, true, false)
		planFinishPreset(r, root, prices, slots, nameOf, unitOf)
		r.tot.StockVal = planStockValue(stock, unitOf)
		switch preset {
		case planPresetFull:
			p.Full = r.tot
		case planPresetOptimized:
			p.Optimized = r.tot
		default:
			p.BuyComp = r.tot
		}
	}
	p.StockVal = p.Optimized.StockVal
	return p
}

// planJitaPrices reads Forge sell rows for exactly the tree's
// types (one bounded read). Missing rows stay missing: no
// price data, never a silent fallback. It returns the map and
// the freshest row stamp.
func (app *Application) planJitaPrices(ctx context.Context, res *buildplan.Result) (map[int64]esi.MarketPrice, time.Time, error) {
	seen := make(map[int64]bool)
	var ids []int64
	buildplan.Walk(res.Root, func(n *buildplan.Node) {
		if !seen[n.TypeID] {
			seen[n.TypeID] = true
			ids = append(ids, n.TypeID)
		}
	})
	rows, err := app.queries.ListMarketTypePricesForTypes(ctx, db.ListMarketTypePricesForTypesParams{
		RegionID: plannerForgeRegionID, Column2: ids,
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	out := make(map[int64]esi.MarketPrice, len(rows))
	var stamp time.Time
	for _, row := range rows {
		if row.SellPrice > 0 {
			out[row.TypeID] = esi.MarketPrice{TypeID: row.TypeID, AveragePrice: row.SellPrice}
			if row.FetchedAt.After(stamp) {
				stamp = row.FetchedAt
			}
		}
	}
	return out, stamp, nil
}

// planMarket picks the price source: Forge sell rows when
// asked (?market=jita), else the cluster guide. The default is
// the guide, so existing links render exactly as before; the
// new tabs link with ?market=jita.
func (app *Application) planMarket(ctx context.Context, q url.Values, res *buildplan.Result, guide map[int64]esi.MarketPrice) (prices map[int64]esi.MarketPrice, market, marketName, pricesNote string) {
	market, marketName = plannerMarketGuide, "Cluster average"
	prices = guide
	if q.Get("market") != plannerMarketJita {
		return prices, market, marketName, "Cluster-average guide prices."
	}
	jita, stamp, err := app.planJitaPrices(ctx, res)
	if err != nil {
		logging.Errorf("planner: jita prices: %v", err)
		return prices, market, marketName, "Jita sell unavailable; cluster average shown."
	}
	market, marketName = plannerMarketJita, "Jita 4-4 sell"
	if stamp.IsZero() {
		return jita, market, marketName, "No Forge sell rows on file yet."
	}
	return jita, market, marketName, "Forge sell rows as of " + stamp.UTC().Format("2006-01-02 15:04") + "."
}

// planBuildSystem resolves the ?system= build system (default
// Jita) and its manufacturing cost index. An unknown ?system=
// falls back to Jita; a system missing from static data keeps
// its numeric name ("System 30000142") while the index lookup
// still runs, so fees work wherever index rows exist. A
// missing index leaves fees unknown.
func (app *Application) planBuildSystem(ctx context.Context, raw string) (systemID int64, systemName string, index float64, indexKnown bool) {
	systemID = plannerDefaultSystemID
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		systemID = n
	}
	if sys, err := app.queries.GetSDESystem(ctx, systemID); err == nil && sys.Name != "" {
		systemName = sys.Name
	} else if name, ok := app.esi.CachedPlaceName(ctx, systemID); ok && name != "" {
		systemName = name
	} else {
		// Honest placeholder, same shape as everywhere else:
		// the warmer fills the cache and the name lands.
		app.notePageWantFromContext(ctx, pageWantPlace, systemID)
		systemName = "System " + strconv.FormatInt(systemID, 10)
	}
	row, err := app.queries.GetIndustryCostIndex(ctx, db.GetIndustryCostIndexParams{
		SolarSystemID: systemID, Activity: plannerCostActivity,
	})
	if err != nil {
		return systemID, systemName, 0, false
	}
	return systemID, systemName, row.CostIndex, true
}

// planAdjustedMap builds the job-fee base from the guide:
// adjusted price where present, average behind it.
func planAdjustedMap(guide map[int64]esi.MarketPrice) map[int64]float64 {
	out := make(map[int64]float64, len(guide))
	for id, p := range guide {
		switch {
		case p.AdjustedPrice > 0:
			out[id] = p.AdjustedPrice
		case p.AveragePrice > 0:
			out[id] = p.AveragePrice
		}
	}
	return out
}

// buildPlanPayloadView computes the interactive blob around a
// plan: market source, build system, skill factor, slots, the
// tree DTO, and canonical preset totals. It re-expands when the
// market source differs from the guide the first pass used;
// the memoized SDE source keeps that cheap and local.
func (app *Application) buildPlanPayloadView(ctx context.Context, q url.Values, src *sdePlannerSource, res *buildplan.Result, runs int64, overrides map[int64]int, owned map[int64]buildplan.OwnedBlueprint, stock map[int64]int64, scoped plannerScopeData, chars []db.Character, productID int64, systemID int64, systemName string, index float64, indexKnown bool, nameOf func(int64) string) (template.JS, *planPayload) {
	guide := app.valuationPrices(ctx)
	prices, market, marketName, pricesNote := app.planMarket(ctx, q, res, guide)
	adjusted := planAdjustedMap(guide)
	if market == plannerMarketJita {
		re, err := buildplan.Build(src, productID, buildplan.Input{
			Runs: runs, MEOverride: overrides, Owned: owned,
			Stock: stock, Prices: prices,
			CostIndex: index, Adjusted: adjusted,
		})
		if err == nil && !re.TooLarge {
			res = re
		}
	}
	factor, timeNote := planTimeFactor(q.Get("timef"), app.planTimeSkills(ctx), scoped, chars)
	slots, slotsNote := planSlots(q.Get("slots"), app.slotSkillIDs(ctx), scoped, chars)
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, productID, prices, unitOf, factor, slots, stock, nameOf)
	p.Market, p.MarketName = market, marketName
	p.SystemID, p.SystemName = systemID, systemName
	p.IndexKnown = indexKnown
	if indexKnown {
		p.IndexPct = index * 100
	}
	p.PricesNote = pricesNote
	p.Slots, p.SlotsNote = slots, slotsNote
	p.TimeNote = timeNote
	raw, err := json.Marshal(p)
	if err != nil {
		logging.Errorf("planner: payload for product %d: %v", productID, err)
		return "", p
	}
	return template.JS(raw), p
}
