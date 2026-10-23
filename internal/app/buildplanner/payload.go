package buildplanner

// Planner payload: one JSON blob per computed plan carrying the
// full build tree with every figure the client may display.
// plan.js mirrors the preset rollup, the schedule and the header
// lines here so a make/buy toggle recomputes live; each mirrored
// function names its twin in plan.js, and a change to either side
// must change both. Each preset's totals ride along; the client
// compares in integer cents and falls back to the server figures
// on any mismatch, so a drifted mirror shows server truth, never an
// invented number.

import (
	"context"
	"encoding/json"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
	"evesynapse/internal/humanize"
	"evesynapse/internal/logging"
	"evesynapse/internal/piplan"
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
	// plannerMarketGuide is the default resolved house map.
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
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	URL        string         `json:"url"`
	Depth      int            `json:"depth"`
	Build      bool           `json:"build"`
	Need       int64          `json:"need"`
	Have       int64          `json:"have"`
	Short      int64          `json:"short"`
	Runs       int64          `json:"runs"`
	Produced   int64          `json:"produced"`
	Surplus    int64          `json:"surplus"`
	ME         int            `json:"me"`
	TE         int            `json:"te"`
	MESource   string         `json:"meSrc"`
	TimeSec    int64          `json:"timeSec"`
	Materials  int64          `json:"materials"`
	Complete   bool           `json:"complete"`
	Unit       int64          `json:"unit"`
	Known      bool           `json:"known"`
	Buy        int64          `json:"buy"`
	Job        int64          `json:"job"`
	JobKnown   bool           `json:"jobKnown"`
	Save       int64          `json:"save"`
	SaveKnown  bool           `json:"saveKnown"`
	Delta      int64          `json:"delta"`
	DeltaOn    bool           `json:"deltaOn"`
	Cycle      bool           `json:"cycle"`
	Capped     bool           `json:"capped"`
	Origin     string         `json:"origin,omitempty"`
	OriginTier string         `json:"originTier,omitempty"`
	Kids       []*planNodeDTO `json:"kids,omitempty"`
}

// planOriginClassifier classifies where a node comes from, for
// the origin line on its card: PI commodities by name (the same
// table the PI planner plans from, tier included), reaction
// products by their SDE group. Anything else is unclassified.
func planOriginClassifier(groups map[int64]string) func(int64, string) (string, string) {
	return func(id int64, name string) (string, string) {
		if tier := piplan.TierOf(name); tier != "" {
			return "pi", string(tier)
		}
		switch groups[id] {
		case "Advanced Materials", "Intermediate Materials", "Polymers",
			"Composite Materials", "Molecular Materials", "Biochemical Materials":
			return "reaction", ""
		}
		return "", ""
	}
}

// planNodeToDTO converts one engine node. factor scales job
// time (trained skills, manual override); money rounds to cents.
func planNodeToDTO(n *buildplan.Node, nameOf func(int64) string, factor float64, originOf func(int64, string) (string, string)) *planNodeDTO {
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
	if originOf != nil && n.Blueprint == nil {
		d.Origin, d.OriginTier = originOf(n.TypeID, d.Name)
	}
	for _, c := range n.Children {
		d.Kids = append(d.Kids, planNodeToDTO(c, nameOf, factor, originOf))
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
	Bought int64 `json:"bought"`
	Built  int64 `json:"built"`
	// RootBought: this scenario buys the finished product
	// outright and builds nothing.
	RootBought bool           `json:"rootBought"`
	Complete   bool           `json:"complete"`
	Unpriced   int            `json:"unpriced"`
	Value      int64          `json:"value"`
	Known      bool           `json:"valueKnown"`
	Profit     int64          `json:"profit"`
	ProfKnown  bool           `json:"profKnown"`
	MarginPct  float64        `json:"marginPct"`
	Elapsed    int64          `json:"elapsed"`
	BuyValue   int64          `json:"buyValue"`
	StockVal   int64          `json:"stockVal"`
	Shopping   []planShopLine `json:"shopping"`
	Jobs       []planJobSpan  `json:"jobs"`
}

// planRollup accumulates one preset's totals over the tree.
// Mirrored by plan.js decide/compute: change both.
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
		if isRoot {
			r.tot.RootBought = true
		} else {
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

// planSchedule (mirrored by plan.js schedule: change both) runs
// deterministic list scheduling over the
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
	Product    int64   `json:"product"`
	Name       string  `json:"name"`
	Produced   int64   `json:"produced"`
	Market     string  `json:"market"`
	MarketName string  `json:"marketName"`
	SystemID   int64   `json:"system"`
	SystemName string  `json:"systemName"`
	IndexPct   float64 `json:"indexPct"`
	IndexKnown bool    `json:"indexKnown"`
	PricesNote string  `json:"pricesNote"`
	Slots      int     `json:"slots"`
	SlotsNote  string  `json:"slotsNote"`
	TimeNote   string  `json:"timeNote"`
	// Owner is the one character the jobs are timed and slotted
	// as (planOwner); OwnerNote is set when the plan spans more
	// characters than that.
	Owner     string           `json:"owner"`
	OwnerNote string           `json:"ownerNote"`
	Root      *planNodeDTO     `json:"root"`
	Full      planPresetTotals `json:"full"`
	Optimized planPresetTotals `json:"optimized"`
	BuyComp   planPresetTotals `json:"buyComp"`
	// StockVal is the priced stockpile behind the hub split.
	// It never depends on the mask, so it sits at top level.
	StockVal int64 `json:"stockVal"`
}

// planTimeSkills resolves the manufacturing time skills' type
// IDs from static data by name (IDs are CCP's to change).
func (app *planner) planTimeSkills(ctx context.Context) map[string]int64 {
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

// planOwner names the one character a plan's jobs are timed and
// slotted as. Job slots are per character in game, and so are the
// skills that shorten a job, so neither may be the best level
// found across several characters. The planner assigns no step to
// anyone in particular, so every step is the owner's.
//
// A single judged character owns its plan. A wider scope (a tag,
// everyone together) pools stock and blueprints, and is owned by
// the acting character when it is in the scope, else by the
// scope's first character.
func planOwner(chars []db.Character, actingID int64) *db.Character {
	for i := range chars {
		if chars[i].CharacterID == actingID {
			return &chars[i]
		}
	}
	if len(chars) > 0 {
		return &chars[0]
	}
	return nil
}

// planOwnerNote says so when a plan spans characters: whose skills
// time and slot the jobs, and that the stock and blueprints are
// pooled. A one-character plan needs no such line.
func planOwnerNote(owner *db.Character, chars []db.Character) string {
	if owner == nil || len(chars) < 2 {
		return ""
	}
	return "Jobs timed and slotted as " + owner.Name + "; stock and blueprints pooled from " + strconv.Itoa(len(chars)) + " characters"
}

// planTimeFactor is the job-time multiplier from the owner's
// trained Industry (4%/level) and Advanced Industry (3%/level),
// multiplicative after TE. Both levels are one character's (see
// planOwner). Unknown skills (or no owner) mean no reduction; a
// manual ?timef= value wins when sane.
func planTimeFactor(raw string, ids map[string]int64, data plannerScopeData, owner *db.Character) (float64, string) {
	if f, err := strconv.ParseFloat(raw, 64); err == nil && f >= 0.05 && f <= 1 {
		return f, "Manual time factor."
	}
	if owner == nil {
		return 1, "No time skills on file: full build times shown."
	}
	trained := data.Skills[owner.CharacterID]
	ind, adv := 0, 0
	if id := ids["Industry"]; id > 0 {
		ind = trained[id]
	}
	if id := ids["Advanced Industry"]; id > 0 {
		adv = trained[id]
	}
	if ind == 0 && adv == 0 {
		return 1, "No time skills on file for " + owner.Name + ": full build times shown."
	}
	factor := (1 - 0.04*float64(ind)) * (1 - 0.03*float64(adv))
	return factor, "Job times use " + owner.Name + "'s Industry and Advanced Industry."
}

// planSlots counts the owner's manufacturing job slots: one plus
// Mass Production plus Advanced Mass Production, both that one
// character's (see planOwner); a manual ?slots= value wins when
// sane.
func planSlots(raw string, ids map[string]int64, data plannerScopeData, owner *db.Character) (int, string) {
	if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 20 {
		return n, "Manual slot count."
	}
	if owner == nil {
		return 1, "One slot: no Mass Production on file."
	}
	trained := data.Skills[owner.CharacterID]
	slots := 1
	if id := ids["Mass Production"]; id > 0 {
		slots += trained[id]
	}
	if id := ids["Advanced Mass Production"]; id > 0 {
		slots += trained[id]
	}
	if slots <= 1 {
		return 1, "One slot: no Mass Production on file for " + owner.Name + "."
	}
	return slots, owner.Name + "'s job slots (Mass Production)."
}

// planFinishPreset (value, profit and margin mirrored by plan.js
// compute: change both) fills value/profit/shopping/schedule around
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
	// An empty preset (the mask buys the finished product, or
	// stock covers everything) must still marshal as [], never
	// null: the client dereferences both lists on first load.
	if t.Shopping == nil {
		t.Shopping = []planShopLine{}
	}
	if t.Jobs == nil {
		t.Jobs = []planJobSpan{}
	}
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

// planHeaderLines (mirrored by plan.js renderHeader: change both)
// formats the optimized preset for the header
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
	elapsed = humanize.Duration(time.Duration(b.Elapsed) * time.Second)
	elapsedSub = "critical path · " + strconv.Itoa(p.Slots) + " slot"
	if p.Slots != 1 {
		elapsedSub += "s"
	}
	return landed, landedSub, value, margin, marginSub, elapsed, elapsedSub
}

// planVerdictBuy is the verdict when the optimized plan buys the
// product: the optimized preset only does that when the market
// price is below the cost of building. plan.js carries the same
// words.
const planVerdictBuy = "Buy it on the market: building costs more"

// planVerdictLine is the build-vs-buy verdict shown above the
// preset buttons: the optimized scenario's profit, worded as
// what building (or buying) saves, with the color class of the
// winning side. The price itself already sits in the header, so
// the line carries only the saving. A scenario that buys the
// finished product has no saving to state (its cost is the
// market price, a margin of exactly zero): the line says to buy.
// plan.js renderHeader rewords it per preset with the same rule.
func planVerdictLine(p *planPayload) (text, cls string) {
	b := p.Optimized
	if b.RootBought {
		return planVerdictBuy, "buy"
	}
	if !b.Known || !b.ProfKnown {
		return "", ""
	}
	if b.Profit >= 0 {
		return "Building saves " + iskCompactCents(b.Profit), "build"
	}
	return "Buying saves " + iskCompactCents(-b.Profit), "buy"
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
func buildPlanPayload(res *buildplan.Result, productID int64, prices map[int64]esi.MarketPrice, unitOf func(int64) (float64, bool), factor float64, slots int, stock map[int64]int64, nameOf func(int64) string, originOf func(int64, string) (string, string)) *planPayload {
	root := res.Root
	p := &planPayload{
		Product: productID, Name: nameOf(productID),
		Produced: root.ProducedQty,
		Root:     planNodeToDTO(root, nameOf, factor, originOf),
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

// planRegionPrices reads one region's sell rows for exactly the
// given types (one bounded read). A type with no row is simply
// absent; planMarket layers what is found over the guide. It
// returns the map and the freshest row stamp.
func (app *planner) planRegionPrices(ctx context.Context, ids []int64, regionID int64) (map[int64]esi.MarketPrice, time.Time, error) {
	rows, err := app.queries.ListMarketTypePricesForTypes(ctx, db.ListMarketTypePricesForTypesParams{
		RegionID: regionID, Column2: ids,
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

// planMarketRegion parses the ?market= selector: "" and
// "guide" are the cluster guide, "jita" is the legacy spelling
// of The Forge, and a bare region id prices from that region.
// Returns 0 for the guide.
func planMarketRegion(raw string) int64 {
	switch strings.TrimSpace(raw) {
	case "", plannerMarketGuide:
		return 0
	case plannerMarketJita:
		return plannerForgeRegionID
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && n > 0 {
		return n
	}
	return 0
}

// resolvePlanPrices settles the planner's one price map from
// the house's own stored sources, per type, in the house order:
//
//  1. The Forge's typical sell, distilled by the market sweeps
//     into market_region_stats: what a buyer actually pays in
//     Jita, with joke orders filtered by the band math.
//  2. The stored estimated sell (true_values), for the items
//     the market cannot price -- thin and contract-traded
//     types. It is the same figure the item page shows, so a
//     plan and the item page cannot disagree about what the
//     product is worth.
//
// A type neither source knows is left out of the map: unpriced,
// never a cluster statistic dressed up as a price. CCP's guide
// prices play no part here -- the guide average lags the market
// too far to plan on, and the adjusted figure is a valuation
// statistic, not a price. The guide still bases job fees, via
// planAdjustedMap, because CCP defines the fee base on it.
// (A stored fallback-aggregate source slots in between the
// estimated sell and unpriced once that feed exists.)
//
// Resolved prices ride in AveragePrice with AdjustedPrice left
// zero, so buildplan.UnitPrice reads exactly the resolved price
// and can never fall through to an adjusted figure.
func resolvePlanPrices(ids []int64, typicals, estSells map[int64]float64) map[int64]esi.MarketPrice {
	out := make(map[int64]esi.MarketPrice, len(ids))
	for _, id := range ids {
		switch {
		case typicals[id] > 0:
			out[id] = esi.MarketPrice{TypeID: id, AveragePrice: typicals[id]}
		case estSells[id] > 0:
			out[id] = esi.MarketPrice{TypeID: id, AveragePrice: estSells[id]}
		}
	}
	return out
}

// planPriceMap reads the two stored price sources for exactly
// the plan's closure types (two bounded reads) and resolves
// the price map. A failed read degrades that source to empty
// -- its types price from the next source, or not at all --
// rather than failing the whole plan.
func (app *planner) planPriceMap(ctx context.Context, ids []int64) map[int64]esi.MarketPrice {
	typicals := make(map[int64]float64, len(ids))
	if rows, err := app.queries.ListMarketRegionStatsForTypes(ctx, db.ListMarketRegionStatsForTypesParams{
		RegionID: plannerForgeRegionID, Column2: ids,
	}); err != nil {
		logging.Errorf("planner: forge stats for plan: %v", err)
	} else {
		for _, row := range rows {
			if row.TypicalSell > 0 {
				typicals[row.TypeID] = row.TypicalSell
			}
		}
	}
	estSells := make(map[int64]float64, len(ids))
	if rows, err := app.queries.ListTrueValuesForTypes(ctx, ids); err != nil {
		logging.Errorf("planner: true values for plan: %v", err)
	} else {
		for _, row := range rows {
			// The stored-estimate invariant (storedEstimate in
			// the app package): a row prices a plan only when
			// an evidence tier stands behind its estimated
			// sell (sell_source names it) -- never at its
			// build cost, or a plan would grade building
			// against the cost of building.
			if row.EstSell.Valid && row.EstSell.Float64 > 0 && row.SellSource != "" {
				estSells[row.TypeID] = row.EstSell.Float64
			}
		}
	}
	return resolvePlanPrices(ids, typicals, estSells)
}

// planMarket picks the price source. The default is the
// resolved house map (planPriceMap): Jita typicals and stored
// estimated sells. A region LAYERS over that base: the region's
// sell price wins where a row is on file, and everything else
// keeps its resolved price -- a thin region thins the basis
// note, never the totals (the old all-or-nothing Jita mode
// zeroed plans whose items had no Forge rows). ids are the
// types the plan can name (the blueprint closure): the source
// is settled before the tree is built, so the tree is built
// once.
func (app *planner) planMarket(ctx context.Context, q url.Values, ids []int64, base map[int64]esi.MarketPrice) (prices map[int64]esi.MarketPrice, market, marketName, pricesNote string) {
	regionID := planMarketRegion(q.Get("market"))
	if regionID == 0 {
		return base, plannerMarketGuide, "Jita market", "Jita sell prices where the market trades; the stored estimated sell for contract-only items."
	}
	market = strconv.FormatInt(regionID, 10)
	marketName = "Region " + market
	if reg, err := app.queries.GetSDERegion(ctx, regionID); err == nil {
		marketName = reg.Name
	}
	rows, stamp, err := app.planRegionPrices(ctx, ids, regionID)
	if err != nil {
		logging.Errorf("planner: region prices: %v", err)
		return base, market, marketName, marketName + " sell unavailable; Jita market shown."
	}
	if stamp.IsZero() {
		return base, market, marketName, marketName + " has no sell rows on file yet; Jita market shown."
	}
	prices = make(map[int64]esi.MarketPrice, len(base))
	for id, p := range base {
		prices[id] = p
	}
	for id, p := range rows {
		prices[id] = p
	}
	return prices, market, marketName, marketName + " sell rows as of " + stamp.UTC().Format("2006-01-02 15:04") + "; Jita market elsewhere."
}

// planBuildSystem resolves the ?system= build system (default
// Jita) and its manufacturing cost index. The field's face is a
// name, so a name resolves first and a numeric ID second; an
// unknown ?system= falls back to Jita. A system missing from
// static data keeps its numeric name ("System 30000142") while
// the index lookup still runs, so fees work wherever index rows
// exist. A missing index leaves fees unknown.
func (app *planner) planBuildSystem(ctx context.Context, raw string) (systemID int64, systemName string, index float64, indexKnown bool) {
	systemID = plannerDefaultSystemID
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil && n > 0 {
			systemID = n
		} else if sys, err := app.queries.GetSDESystemByName(ctx, trimmed); err == nil {
			systemID = sys.SystemID
		}
	}
	if sys, err := app.queries.GetSDESystem(ctx, systemID); err == nil && sys.Name != "" {
		systemName = sys.Name
	} else if name, ok := app.deps.CachedPlaceName(ctx, systemID); ok && name != "" {
		systemName = name
	} else {
		// Honest placeholder, same shape as everywhere else:
		// the warmer fills the cache and the name lands.
		app.deps.NotePlaceWant(ctx, systemID)
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

// The SDE dogma attributes carrying facility bonuses. The
// engineering complexes publish their role bonus as straight
// multipliers (strEng*); rigs publish percent decreases
// (2593/2594) plus per-security-band effectiveness multipliers
// (2355/2356/2357). The values are uniform across every
// manufacturing rig family of a tier (verified against the
// SDE), so one representative rig per tier and kind speaks for
// the family the pilot would actually fit.
const (
	attrStrEngMatBonus  = 2600
	attrStrEngCostBonus = 2601
	attrStrEngTimeBonus = 2602
	attrRigTimeBonus    = 2593
	attrRigMatBonus     = 2594
	attrRigSecHi        = 2355
	attrRigSecLow       = 2356
	attrRigSecNull      = 2357
)

var (
	rigRepresentativeME = map[string]int64{"t1": 37146, "t2": 37147} // Standup M-Set Basic Medium Ship ME I/II
	rigRepresentativeTE = map[string]int64{"t1": 37162, "t2": 37153} // Standup M-Set Basic Small/Medium Ship TE I/II
	plannerStructures   = map[int64]string{35825: "Raitaru", 35826: "Azbel", 35827: "Sotiyo"}
)

// planFacility resolves the ?structure= / ?rig= picks into the
// engine's facility multipliers, all read from the local SDE:
// the structure's role bonus, and the rig tier's bonuses scaled
// by the build system's security band exactly as the rig's own
// attributes prescribe. No structure (or an unknown one) is an
// NPC station: every factor 1 and no note. Rigs need a
// structure to hang on.
func (app *planner) planFacility(ctx context.Context, rawStructure, rawRig string, systemID int64) (matFactor, timeFactor, costFactor float64, note string, structureID int64, rigTier string) {
	matFactor, timeFactor, costFactor = 1, 1, 1
	structureID, _ = strconv.ParseInt(strings.TrimSpace(rawStructure), 10, 64)
	name, ok := plannerStructures[structureID]
	if !ok {
		return matFactor, timeFactor, costFactor, "", 0, ""
	}
	rigTier = strings.ToLower(strings.TrimSpace(rawRig))
	meRig, teRig := rigRepresentativeME[rigTier], rigRepresentativeTE[rigTier]
	if meRig == 0 {
		rigTier, meRig, teRig = "", 0, 0
	}
	ids := []int64{structureID}
	if meRig > 0 {
		ids = append(ids, meRig, teRig)
	}
	attrs := make(map[int64]map[int64]float64)
	if rows, err := app.queries.ListSDETypeAttributesByIDs(ctx, ids); err == nil {
		for _, row := range rows {
			if attrs[row.TypeID] == nil {
				attrs[row.TypeID] = make(map[int64]float64)
			}
			attrs[row.TypeID][row.AttributeID] = row.Value
		}
	} else {
		logging.Errorf("planner: facility attributes: %v", err)
	}
	get := func(typeID, attr int64, def float64) float64 {
		if v, ok := attrs[typeID][attr]; ok && v != 0 {
			return v
		}
		return def
	}
	matFactor = get(structureID, attrStrEngMatBonus, 1)
	costFactor = get(structureID, attrStrEngCostBonus, 1)
	timeFactor = get(structureID, attrStrEngTimeBonus, 1)
	note = name
	if meRig > 0 {
		// The rig's own security multipliers, applied to the
		// build system's band; an unknown system earns the
		// high-sec multiplier, never a better one.
		secAttr := int64(attrRigSecHi)
		if sys, err := app.queries.GetSDESystem(ctx, systemID); err == nil {
			switch {
			case sys.Security <= 0:
				secAttr = attrRigSecNull
			case sys.Security < 0.5:
				secAttr = attrRigSecLow
			}
		}
		secMult := get(meRig, secAttr, 1)
		matFactor *= 1 + get(meRig, attrRigMatBonus, 0)/100*secMult
		timeFactor *= 1 + get(teRig, attrRigTimeBonus, 0)/100*secMult
		note += " · " + strings.ToUpper(rigTier) + " rigs"
	}
	return matFactor, timeFactor, costFactor, note, structureID, rigTier
}

// planJobTaxes resolves the job-fee terms that are neither the
// system index nor a structure bonus (buildplan.JobFee): the
// facility tax and whether an Alpha clone installs the job.
//
// The facility tax is 0.25% at an NPC station. At a player
// structure its owner sets it and nothing public says what it
// is, so it is whatever ?ftax= gives (a percentage) and zero
// until then; ?ftax= also overrides the NPC rate. Clone state is
// not in any ESI data the app holds either, so the Alpha tax is
// a setting too (?alpha=1), off by default.
func planJobTaxes(q url.Values, structureID int64) (facilityTax float64, alpha bool, note string) {
	facilityTax = buildplan.NPCFacilityTax
	if structureID != 0 {
		facilityTax = 0
	}
	if pct, err := strconv.ParseFloat(strings.TrimSpace(q.Get("ftax")), 64); err == nil && pct >= 0 && pct <= 100 {
		facilityTax = pct / 100
	}
	alpha = q.Get("alpha") == "1"
	note = "facility tax " + strconv.FormatFloat(facilityTax*100, 'f', -1, 64) + "% · SCC surcharge " +
		strconv.FormatFloat(buildplan.SCCSurcharge*100, 'f', -1, 64) + "%"
	if alpha {
		note += " · Alpha clone tax " + strconv.FormatFloat(buildplan.AlphaCloneTax*100, 'f', -1, 64) + "%"
	}
	return facilityTax, alpha, note
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

// planOriginOf builds the origin classifier for one plan: one
// batched group lookup over the tree's types, then the pure
// classifier. A failed lookup degrades to PI-only origins.
func (app *planner) planOriginOf(ctx context.Context, root *buildplan.Node) func(int64, string) (string, string) {
	seen := make(map[int64]bool)
	var ids []int64
	buildplan.Walk(root, func(n *buildplan.Node) {
		if !seen[n.TypeID] {
			seen[n.TypeID] = true
			ids = append(ids, n.TypeID)
		}
	})
	groups := make(map[int64]string, len(ids))
	if rows, err := app.queries.ListSDETypeGroups(ctx, ids); err == nil {
		for _, r := range rows {
			groups[r.TypeID] = r.GroupName
		}
	}
	return planOriginClassifier(groups)
}

// planPayloadArgs is a built plan and what buildPlanPayloadView
// labels it with: the request, the prices and market it was built
// with, the scope's stock and characters, and the build system.
type planPayloadArgs struct {
	Query                          url.Values
	Result                         *buildplan.Result
	Prices                         map[int64]esi.MarketPrice
	Market, MarketName, PricesNote string
	Stock                          map[int64]int64
	Scoped                         plannerScopeData
	Chars                          []db.Character
	ProductID                      int64
	SystemID                       int64
	SystemName                     string
	Index                          float64
	IndexKnown                     bool
	NameOf                         func(int64) string
}

// buildPlanPayloadView shapes the interactive blob around a
// built plan: skill factor, slots, the tree DTO and the canonical
// preset totals, labelled with the market and build system the
// caller built it with. It never expands the tree again.
func (app *planner) buildPlanPayloadView(ctx context.Context, a planPayloadArgs) (template.JS, *planPayload) {
	q, res, prices, stock, scoped, chars := a.Query, a.Result, a.Prices, a.Stock, a.Scoped, a.Chars
	market, marketName, pricesNote := a.Market, a.MarketName, a.PricesNote
	productID, systemID, systemName := a.ProductID, a.SystemID, a.SystemName
	index, indexKnown, nameOf := a.Index, a.IndexKnown, a.NameOf
	owner := planOwner(chars, app.deps.SessionCharacterID(ctx))
	factor, timeNote := planTimeFactor(q.Get("timef"), app.planTimeSkills(ctx), scoped, owner)
	slots, slotsNote := planSlots(q.Get("slots"), app.deps.SlotSkillIDs(ctx), scoped, owner)
	unitOf := func(id int64) (float64, bool) { return buildplan.UnitPrice(prices, id) }
	p := buildPlanPayload(res, productID, prices, unitOf, factor, slots, stock, nameOf, app.planOriginOf(ctx, res.Root))
	p.Market, p.MarketName = market, marketName
	p.SystemID, p.SystemName = systemID, systemName
	p.IndexKnown = indexKnown
	if indexKnown {
		p.IndexPct = index * 100
	}
	p.PricesNote = pricesNote
	p.Slots, p.SlotsNote = slots, slotsNote
	p.TimeNote = timeNote
	if owner != nil {
		p.Owner = owner.Name
	}
	p.OwnerNote = planOwnerNote(owner, chars)
	raw, err := json.Marshal(p)
	if err != nil {
		logging.Errorf("planner: payload for product %d: %v", productID, err)
		return "", p
	}
	return template.JS(raw), p
}
