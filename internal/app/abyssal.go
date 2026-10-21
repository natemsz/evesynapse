package app

// Abyssal scoring and estimates (schema 057): what a mutated
// module's rolls are worth, computed by the worker and stored
// on abyssal_items, so the appraisal core and the contract
// pages only ever read finished numbers.
//
// The method, in order. Every rolled attribute is placed in
// the multiplier band its mutaplasmid can produce (the FSD
// ranges, schema 055) as a direction-aware percentile: where
// the roll sits between the worst and best outcome that
// mutaplasmid could have made. A percentile is then scaled
// by the width of its band, so a near-perfect roll on a
// narrow band barely speaks and the same percentile on a
// wide band speaks loudly. Family weights say which
// attributes carry the module's value; fitting attributes
// (CPU, powergrid and their kin) are penalty-only: a good
// fitting roll earns nothing, a bad one subtracts, and a
// bricked one caps the score. A bricked primary attribute
// caps the score whatever the rest rolled, and the cap and
// the attribute that set it are stored with the estimate.
//
// The estimate prices the item, not the market's opinion of
// it. Every family has a ladder of ordinary modules -- Tech II,
// faction, deadspace, officer: the source module's SDE group,
// its size class -- whose stats are fixed game data and whose
// prices come from markets far deeper than abyssal contracts.
// The item and every rung are placed on one performance index
// (the scorer's weighted positions and fitting drag, measured
// against the item's own source base and bands, unclamped), and
// the item is priced between the rungs it falls between. Where
// it falls is a fact of its absolute stats, so identical stats
// mutated from different sources price the same. Recreation
// cost -- the cheapest path to roll at least this profile again
// over every base module and mutaplasmid grade seen making the
// type, each on its own odds, every missed roll's resale
// credited at a haircut -- caps the figure: nobody needs to pay
// more than making one. With no priced ladder, the source
// module's price run through the score curve speaks instead,
// with a bounded correction fitted from listings.
//
// Contract listings never set a figure alone. A thin family's
// listings only nudge one per-family factor, clamped to
// [0.8, 1.25] and shrunk toward no correction until enough clean
// listings have spoken (abyssalMarketCalibration). Where a
// family is dense enough, a monotone boosted correction learned
// from its clean listings and confirmed sales replaces the
// factor, within [0.6, 1.6] of the ladder -- but only when it
// beat the factor on newer listings it never saw
// (abyssal_learn.go). Clean means: a listing's price is
// attributed to the item (bundles of abyssals and exchanges are
// refused, small priced passengers subtracted -- schema 069),
// weighted by how it left the market (a sale ESI confirmed
// counts most -- schema 070, pub_contracts_sales.go -- and
// sitting out its term least), by recency and by how long it
// has sat live, with each seller's say growing only as the
// square root of what they list; and a listing priced
// suspiciously close to our own estimate is down-weighted. An
// item's own ask is never an input to its estimate.
//
// Estimates are honest about absence. No ranges, no score.
// No score, or nothing to anchor on (no corpus bucket and no
// source price), no ISK figure: the item shows its rolls and
// percentiles and says why there is no number.

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/logging"
)

const (
	// abyssalScoreBatch caps one pass; the corpus is probed
	// over days, and the pass runs every cycle.
	abyssalScoreBatch = 200
	// abyssalRescoreAge: stored scores refresh hourly, the
	// cadence the appraisal pass reprices contracts on, so a
	// growing corpus improves estimates within the hour.
	abyssalRescoreAge = time.Hour
)

// abyssalBandRef is the multiplier-band width at which an
// attribute speaks at full weight: a band spanning 60% of
// the base value is a wide one. Narrower bands scale down
// linearly.
const abyssalBandRef = 0.6

// Primary gating: a primary attribute at percentile p caps
// the score at floor + (1-floor)*p.
const (
	abyssalCapFloor     = 0.2
	abyssalFitCap       = 0.45 // a deeply bricked fitting attribute caps the score here
	abyssalFitBrickPct  = 0.12 // direction-adjusted percentile below which a fitting roll is a brick
	abyssalComboCap     = 0.55 // several fitting bricks together cap here
	abyssalComboDeficit = 1.2  // summed fitting deficit that counts as a combination brick
)

// The score curve: what multiplier over the source module's
// price a score is worth before calibration. Piecewise
// linear through these points; a mid roll is worth about
// the base module, a bricked one less, and the curve opens
// up steeply at the top, where scarcity actually lives.
var abyssalCurvePoints = [][2]float64{
	{0.00, 0.30}, {0.25, 0.55}, {0.50, 1.00}, {0.70, 1.90},
	{0.85, 3.60}, {0.95, 6.50}, {1.00, 10.00},
}

func abyssalCurveMult(score float64) float64 {
	s := clamp01Abyssal(score)
	for i := 1; i < len(abyssalCurvePoints); i++ {
		hi := abyssalCurvePoints[i]
		if s <= hi[0] {
			lo := abyssalCurvePoints[i-1]
			if hi[0] == lo[0] {
				return hi[1]
			}
			return lo[1] + (hi[1]-lo[1])*(s-lo[0])/(hi[0]-lo[0])
		}
	}
	return abyssalCurvePoints[len(abyssalCurvePoints)-1][1]
}

func clamp01Abyssal(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// abyssalRange is one mutaplasmid band for one attribute.
type abyssalRange struct {
	MinMult float64
	MaxMult float64
}

// abyssalAttrResult is one rolled attribute's scoring facts.
type abyssalAttrResult struct {
	AttributeID int64
	Percentile  float64 // direction-adjusted, 0..1
	Weight      float64 // family weight (0 when the family does not score it)
	EffWeight   float64 // weight after band-width scaling
	PenaltyOnly bool
	Primary     bool
	Scored      bool // the family scores this attribute and a percentile exists
}

// abyssalDerived is one derived figure (repair per second
// and kin) with its own percentile in its reachable band.
type abyssalDerived struct {
	Key        string  `json:"key"`
	Label      string  `json:"label"`
	Value      float64 `json:"value"`
	Percentile float64 `json:"pct"`
}

// abyssalScoring is the pure scorer's output.
type abyssalScoring struct {
	HasScore       bool
	Score          float64
	Attrs          []abyssalAttrResult // every ranged roll, by attribute id
	Percentiles    map[int64]float64   // every ranged roll's percentile
	Derived        []abyssalDerived
	CapKind        string // "", "primary", "fitting"
	CapAttributeID int64
	Attempts       float64 // expected attempts to re-roll this profile; 0 when unusable
	// PrimaryAbs is the item's absolute primary performance
	// figure: the derived stat's rolled value when the family
	// scores one, else the rolled value of the highest-weight
	// primary roll. Corpus bucketing matches on it (brief
	// section 9e: buckets are absolute performance profiles,
	// and families like Shield Booster span size classes whose
	// boost amounts differ by an order of magnitude).
	PrimaryAbs float64
}

// abyssalScoringInputs is everything the pure scorer reads.
type abyssalScoringInputs struct {
	Family   string
	Rolls    map[int64]float64        // attribute id -> rolled value
	Ranges   map[int64]abyssalRange   // attribute id -> this mutaplasmid's band
	Base     map[int64]float64        // attribute id -> source module's base value
	HighGood map[int64]bool           // attribute id -> high is good (default true)
	Weights  []db.AbyssalFamilyWeight // the family's weights; empty = unseeded family
}

// abyssalAttrPercentile places a rolled value in its band:
// the direction-adjusted percentile, and whether the inputs
// support one at all (a zero base or an empty band cannot).
func abyssalAttrPercentile(rolled, base float64, r abyssalRange, highGood bool) (float64, bool) {
	if base == 0 {
		return 0, false
	}
	span := r.MaxMult - r.MinMult
	if span <= 0 {
		return 0, false
	}
	pos := clamp01Abyssal((rolled/base - r.MinMult) / span)
	if !highGood {
		pos = 1 - pos
	}
	return pos, true
}

// abyssalBandScale is how loudly an attribute speaks: its
// band's width against the reference width.
func abyssalBandScale(r abyssalRange) float64 {
	w := (r.MaxMult - r.MinMult) / abyssalBandRef
	if w > 1 {
		return 1
	}
	if w < 0 {
		return 0
	}
	return w
}

// scoreAbyssal is the pure scorer. It is monotone by
// construction: improving any roll (moving its
// direction-adjusted percentile up), all else equal, never
// lowers the score. Percentiles come first and exist even
// when the family has no weights (the rolls still display);
// the score needs the weights.
func scoreAbyssal(in abyssalScoringInputs) abyssalScoring {
	out := abyssalScoring{Percentiles: map[int64]float64{}}
	highGood := make(map[int64]bool, len(in.Rolls))
	for id := range in.Rolls {
		highGood[id] = true
		if hg, ok := in.HighGood[id]; ok {
			highGood[id] = hg
		}
	}
	ids := make([]int64, 0, len(in.Rolls))
	for id := range in.Rolls {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Percentiles for every roll the mutaplasmid can move.
	for _, id := range ids {
		rng, ok := in.Ranges[id]
		if !ok {
			continue
		}
		base, ok := in.Base[id]
		if !ok {
			continue
		}
		high := highGood[id]
		pct, ok := abyssalAttrPercentile(in.Rolls[id], base, rng, high)
		if !ok {
			continue
		}
		out.Percentiles[id] = pct
		out.Attrs = append(out.Attrs, abyssalAttrResult{AttributeID: id, Percentile: pct})
	}

	// Derived figures: where a family's value is a ratio, the
	// ratio is what gets scored. Repair per second replaces
	// the raw amount percentile in the weighted sum below.
	derivedPctByAttr := map[int64]float64{}
	if d, amountAttr, ok := abyssalDerivedStat(in); ok {
		out.Derived = []abyssalDerived{d}
		derivedPctByAttr[amountAttr] = d.Percentile
	}

	if len(in.Weights) == 0 {
		return out
	}
	weightBy := make(map[int64]db.AbyssalFamilyWeight, len(in.Weights))
	maxPrimary := 0.0
	for _, w := range in.Weights {
		weightBy[w.AttributeID] = w
		if !w.PenaltyOnly && w.Weight > maxPrimary {
			maxPrimary = w.Weight
		}
	}

	var sumW, sumWV float64
	var penaltyDrag, deficitSum float64
	type capCand struct {
		pct  float64
		attr int64
	}
	var primaries []capCand
	var fitBricks []int64
	for i := range out.Attrs {
		a := &out.Attrs[i]
		w, ok := weightBy[a.AttributeID]
		if !ok {
			continue
		}
		rng := in.Ranges[a.AttributeID]
		a.Weight = w.Weight
		a.PenaltyOnly = w.PenaltyOnly
		a.EffWeight = w.Weight * abyssalBandScale(rng)
		a.Scored = true
		a.Primary = !w.PenaltyOnly && w.Weight >= maxPrimary && maxPrimary > 0
		pct := a.Percentile
		if dp, ok := derivedPctByAttr[a.AttributeID]; ok {
			pct = dp // the ratio speaks for the amount attribute
		}
		if w.PenaltyOnly {
			// The percentile of the unmutated value in this
			// band: rolls at or better than it cost nothing.
			basePctAdj := abyssalBasePercentile(rng, highGood[a.AttributeID])
			deficit := basePctAdj - pct
			if deficit < 0 {
				deficit = 0
			}
			deficitSum += deficit
			penaltyDrag += a.EffWeight * deficit
			if pct < abyssalFitBrickPct {
				fitBricks = append(fitBricks, a.AttributeID)
			}
			continue
		}
		sumW += a.EffWeight
		sumWV += a.EffWeight * pct
		if a.Primary {
			primaries = append(primaries, capCand{pct: pct, attr: a.AttributeID})
		}
	}
	if sumW <= 0 {
		return out // the family scores nothing this item rolled
	}
	out.HasScore = true
	score := sumWV / sumW
	// Penalty drag, normalized against every scored weight
	// (penalties included), so a fitting brick visibly dents
	// even a perfect roll set.
	totalW := sumW
	for i := range out.Attrs {
		if out.Attrs[i].Scored && out.Attrs[i].PenaltyOnly {
			totalW += out.Attrs[i].EffWeight
		}
	}
	if totalW > 0 {
		score -= penaltyDrag / totalW
	}

	// Gates. The lowest binding cap wins and names itself.
	capVal := math.Inf(1)
	capKind := ""
	var capAttr int64
	for _, p := range primaries {
		if c := abyssalCapFloor + (1-abyssalCapFloor)*p.pct; c < capVal {
			capVal, capKind, capAttr = c, "primary", p.attr
		}
	}
	if len(fitBricks) > 0 && abyssalFitCap < capVal {
		capVal, capKind, capAttr = abyssalFitCap, "fitting", fitBricks[0]
	}
	if deficitSum > abyssalComboDeficit && abyssalComboCap < capVal {
		capVal, capKind = abyssalComboCap, "fitting"
		capAttr = 0
		for i := range out.Attrs {
			if out.Attrs[i].Scored && out.Attrs[i].PenaltyOnly {
				capAttr = out.Attrs[i].AttributeID // the first fitting contributor
				break
			}
		}
	}
	if capVal < score {
		score = capVal
		out.CapKind, out.CapAttributeID = capKind, capAttr
	}
	out.Score = clamp01Abyssal(score)
	if len(out.Derived) > 0 {
		out.PrimaryAbs = out.Derived[0].Value
	} else {
		bestW := 0.0
		for i := range out.Attrs {
			a := &out.Attrs[i]
			if a.Scored && a.Primary && a.EffWeight > bestW {
				bestW = a.EffWeight
				out.PrimaryAbs = in.Rolls[a.AttributeID]
			}
		}
	}

	// Recreation survival: the share-weighted chance that one
	// fresh roll matches or beats this profile on every
	// scored attribute. Survival per attribute is 1 - pct by
	// construction of the direction-adjusted percentile.
	logP := 0.0
	for i := range out.Attrs {
		a := &out.Attrs[i]
		if !a.Scored || totalW <= 0 {
			continue
		}
		pct := a.Percentile
		if dp, ok := derivedPctByAttr[a.AttributeID]; ok {
			pct = dp
		}
		survive := 1 - pct
		if survive < 0.001 {
			survive = 0.001
		}
		logP += (a.EffWeight / totalW) * math.Log(survive)
	}
	if logP < 0 {
		out.Attempts = math.Exp(-logP)
		if out.Attempts > 100000 {
			out.Attempts = 100000
		}
		if out.Attempts < 1 {
			out.Attempts = 1
		}
	}
	return out
}

// abyssalBasePercentile is the direction-adjusted percentile
// of the unmutated value (multiplier exactly 1) in a band:
// the neutral point fitting deficits are measured from.
func abyssalBasePercentile(r abyssalRange, highGood bool) float64 {
	span := r.MaxMult - r.MinMult
	if span <= 0 {
		return 0.5
	}
	pos := clamp01Abyssal((1 - r.MinMult) / span)
	if !highGood {
		pos = 1 - pos
	}
	return pos
}

// The derived-stat families: value is amount per second, not
// amount per cycle. Amount attribute 84 (armor) or 68
// (shield), cycle attribute 73 (duration, milliseconds).
var abyssalDerivedFamilies = map[string]int64{
	"armor_repair_unit":        84,
	"ancillary_armor_repairer": 84,
	"remote_armor_repairer":    84,
	"shield_booster":           68,
	"ancillary_shield_booster": 68,
}

const abyssalDurationAttr = 73

// abyssalDerivedStat computes the family's ratio figure for
// an item: its value, and its percentile between the worst
// ratio the mutaplasmid could produce (least amount, longest
// cycle) and the best (most amount, shortest cycle).
func abyssalDerivedStat(in abyssalScoringInputs) (abyssalDerived, int64, bool) {
	amountAttr, ok := abyssalDerivedFamilies[in.Family]
	if !ok {
		return abyssalDerived{}, 0, false
	}
	amount, okA := in.Rolls[amountAttr]
	dur, okD := in.Rolls[abyssalDurationAttr]
	baseA, okBA := in.Base[amountAttr]
	baseD, okBD := in.Base[abyssalDurationAttr]
	rngA, okRA := in.Ranges[amountAttr]
	rngD, okRD := in.Ranges[abyssalDurationAttr]
	if !okA || !okD || !okBA || !okBD || !okRA || !okRD || baseA == 0 || baseD == 0 || dur <= 0 {
		return abyssalDerived{}, 0, false
	}
	perSec := func(amount, durMs float64) float64 { return amount / (durMs / 1000) }
	value := perSec(amount, dur)
	worst := perSec(baseA*rngA.MinMult, baseD*rngD.MaxMult)
	best := perSec(baseA*rngA.MaxMult, baseD*rngD.MinMult)
	if best <= worst {
		return abyssalDerived{}, 0, false
	}
	label := "Repair per second"
	if amountAttr == 68 {
		label = "Shield boost per second"
	}
	return abyssalDerived{
		Key:        "per_second",
		Label:      label,
		Value:      value,
		Percentile: clamp01Abyssal((value - worst) / (best - worst)),
	}, amountAttr, true
}

// ---------------------------------------------------------------------
// The estimate: corpus bands first, the score curve behind
// them, recreation stored alongside as a cross-check.
// ---------------------------------------------------------------------

// abyssalComp is one corpus comparable: a scored item of the
// family, its attributed ask, its source module price (for the
// curve correction), its uncalibrated ladder figure (for the
// market calibration), and its prior weight. The item being
// estimated is never among its own comps: callers exclude it.
type abyssalComp struct {
	ItemID        int64
	SourceTypeID  int64
	Score         float64
	Ask           float64
	SourceTypical float64
	// LadderValue is the comp's own uncalibrated ladder figure
	// from its last scoring; 0 when it has none.
	LadderValue float64
	// DynamicsWeight is the comp's prior say: exit dynamics,
	// recency, attribution and the seller's share (compsFor).
	DynamicsWeight float64
	PrimaryAbs     float64
	// For the learned correction: the comp's rolls and the
	// mutaplasmid that made them, when it was first listed, and
	// whether its listing is a confirmed sale.
	Rolls         json.RawMessage
	MutatorTypeID int64
	FirstSeen     time.Time
	Sold          bool
}

// abyssalRung is one ordinary module on an item's ladder: its
// type, our price for it, and its performance index measured
// exactly as the item's is (abyssalLadderIndex, against the
// item's own source base and mutaplasmid bands).
type abyssalRung struct {
	TypeID int64
	Price  float64
	Index  float64
}

// abyssalEstimateInput is the estimator's whole world.
type abyssalEstimateInput struct {
	Score         float64
	SourceTypical float64 // the item's own source module price; 0 = none
	SourceTypeID  int64
	PrimaryAbs    float64
	// Index is the item's ladder index; HasIndex is false when
	// the family scores nothing the item rolled.
	Index    float64
	HasIndex bool
	Rungs    []abyssalRung // priced, same size class
	// Recreation is the expected cost to roll another at
	// least this good; 0 when unknown. It caps the figure.
	Recreation float64
	Comps      []abyssalComp // same family, the item itself excluded
	// Learned is the family's learned correction for this item
	// (abyssal_learn.go); 0 when the family's model is not in
	// use, and the bounded factor speaks instead.
	Learned float64
	// ErrFloor is the family's measured out-of-sample median
	// log error for whichever adjustment applies (0: none yet).
	// The range is never narrower than it.
	ErrFloor float64
}

// abyssalEstimate is one stored estimate.
type abyssalEstimate struct {
	OK    bool
	Value float64
	Low   float64
	High  float64
	// Basis: "ladder", "curve" or "prior". Scope, for the
	// ladder: "between" two rungs, "above" the top rung,
	// "below" the bottom one, or "single" (one priced rung).
	Basis       string
	Scope       string
	Comps       int // ladder: priced rungs; curve: listings
	PremiumMult float64
	LadderValue float64 // the uncalibrated ladder figure
	LadderLo    int64   // the rung just below (0: none)
	LadderHi    int64   // the rung just above (0: none)
	Calib       float64 // the multiplier applied to the ladder figure
	// MarketBasis names that multiplier: "factor" (the bounded
	// per-family factor) or "learned" (the learned correction).
	MarketBasis string
}

type weightedValue struct {
	v float64
	w float64
}

// abyssalWeightedQuantile: the value at cumulative weight
// fraction q of the weight-sorted values.
func abyssalWeightedQuantile(vals []weightedValue, q float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]weightedValue(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].v < sorted[j].v })
	total := 0.0
	for _, x := range sorted {
		total += x.w
	}
	if total <= 0 {
		return sorted[len(sorted)/2].v
	}
	target := q * total
	cum := 0.0
	for _, x := range sorted {
		cum += x.w
		if cum >= target {
			return x.v
		}
	}
	return sorted[len(sorted)-1].v
}

// ---------------------------------------------------------------------
// The ladder. An abyssal module is priced as an item, not by
// what other abyssals were listed for: every family has a
// ladder of ordinary modules (Tech II, faction, deadspace,
// officer -- the same SDE group as the source module, the same
// size class), whose stats are fixed game data and whose prices
// come from markets far deeper than abyssal contracts. The item
// and every rung are placed on one performance index -- the
// family's weighted roll positions, measured against the item's
// own source base and mutaplasmid bands, unclamped so a rung
// beyond the band still ranks above one inside it -- and the
// item is priced between the rungs it falls between, in log
// space (rung prices made non-decreasing in index first, so a
// better module never prices lower). The ladder is closed from
// above by the perfect roll -- every stat at its band's good end,
// priced at what making one would cost (abyssalPerfectRung) -- so
// an item past the top ordinary rung prices between it and the
// perfect roll; with no priced perfect roll, the top rung is the
// price. Below the bottom rung the first step extends, to at most
// a tenth of that rung.
// Because the item and the rungs are measured against the same
// base, where the item falls between two rungs is a fact of its
// absolute stats: identical stats from a different source
// module land in the same place.
//
// Recreation cost (rolling another at least this good, at base
// module and mutaplasmid prices) caps the figure: nobody needs
// to pay more than making one. Contract listings never set the
// figure; cleaned of bundles, exchanges, stale and one-seller
// floods, they nudge one bounded per-family factor, learned
// slowly (abyssalMarketCalibration).
//
// The range is the ladder's own uncertainty where the item sits
// -- tight next to a rung, widest midway across a big price gap,
// wider still past the ends -- plus the calibration's.
// ---------------------------------------------------------------------

const (
	abyssalCalibMin     = 0.8  // the market factor's floor
	abyssalCalibMax     = 1.25 // and its ceiling
	abyssalCalibPrior   = 20.0 // effective listings the factor needs to move halfway
	abyssalRungRatioMin = 0.55 // size class: a rung's primary figure against the source base's
	abyssalRungRatioMax = 1.8
	abyssalQuartileZ    = 0.6745
	// abyssalLadderStep is the log-price step per index unit
	// used below the bottom rung when the ladder has a single
	// rung: about the curve's own steepness at mid-range. A
	// fitted end step is held within [0.25x, 4x] of it, so two
	// rungs at nearly the same index cannot make a vertical one.
	abyssalLadderStep = 2.0
	// abyssalBelowMaxDrop: below the bottom rung the price falls
	// at most to a tenth of that rung's (log 10).
	abyssalBelowMaxDrop = 2.302585092994046
	// abyssalAboveSD is the log uncertainty of an item past the
	// top rung, which prices at that rung: it is at least that
	// good, and how much better the ladder cannot say.
	abyssalAboveSD = 0.25
	// abyssalLadderSaneMax bounds a stored ladder figure the
	// corpus will read: nothing the game trades comes near it,
	// so a figure past it is a pre-clamp extrapolation awaiting
	// its rescore, and reads as missing.
	abyssalLadderSaneMax = 1e13
)

// abyssalLadderIndex is the performance index of a set of
// attribute values (an item's rolls, or a rung's base stats)
// against the item's source base and mutaplasmid bands: the
// scorer's weighted positions and fitting drag, unclamped and
// without caps. ok is false when the family scores none of the
// attributes present.
func abyssalLadderIndex(in abyssalScoringInputs, vals map[int64]float64) (float64, bool) {
	if len(in.Weights) == 0 {
		return 0, false
	}
	pos := func(id int64, v float64) (float64, bool) {
		rng, ok := in.Ranges[id]
		if !ok {
			return 0, false
		}
		base := in.Base[id]
		span := rng.MaxMult - rng.MinMult
		if base == 0 || span <= 0 {
			return 0, false
		}
		p := (v/base - rng.MinMult) / span
		hg := true
		if h, ok := in.HighGood[id]; ok {
			hg = h
		}
		if !hg {
			p = 1 - p
		}
		return p, true
	}
	// The derived families score amount per second, not amount.
	derived := map[int64]float64{}
	if amountAttr, ok := abyssalDerivedFamilies[in.Family]; ok {
		amount, okA := vals[amountAttr]
		dur, okD := vals[abyssalDurationAttr]
		baseA, baseD := in.Base[amountAttr], in.Base[abyssalDurationAttr]
		rngA, okRA := in.Ranges[amountAttr]
		rngD, okRD := in.Ranges[abyssalDurationAttr]
		if okA && okD && okRA && okRD && baseA != 0 && baseD != 0 && dur > 0 {
			perSec := func(a, d float64) float64 { return a / (d / 1000) }
			worst := perSec(baseA*rngA.MinMult, baseD*rngD.MaxMult)
			best := perSec(baseA*rngA.MaxMult, baseD*rngD.MinMult)
			if best > worst {
				derived[amountAttr] = (perSec(amount, dur) - worst) / (best - worst)
			}
		}
	}
	var sumW, sumWV, drag, penW float64
	for _, w := range in.Weights {
		v, ok := vals[w.AttributeID]
		if !ok {
			continue
		}
		p, ok := pos(w.AttributeID, v)
		if !ok {
			continue
		}
		if dp, ok := derived[w.AttributeID]; ok {
			p = dp
		}
		rng := in.Ranges[w.AttributeID]
		ew := w.Weight * abyssalBandScale(rng)
		if w.PenaltyOnly {
			hg := true
			if h, ok := in.HighGood[w.AttributeID]; ok {
				hg = h
			}
			if d := abyssalBasePercentile(rng, hg) - p; d > 0 {
				drag += ew * d
			}
			penW += ew
			continue
		}
		sumW += ew
		sumWV += ew * p
	}
	if sumW <= 0 {
		return 0, false
	}
	return sumWV/sumW - drag/(sumW+penW), true
}

// abyssalPrimaryFigure is the size-class figure of a set of
// values: amount per second for the derived families, else the
// family's highest-weight primary attribute. 0 when absent.
func abyssalPrimaryFigure(in abyssalScoringInputs, vals map[int64]float64) float64 {
	if amountAttr, ok := abyssalDerivedFamilies[in.Family]; ok {
		a, d := vals[amountAttr], vals[abyssalDurationAttr]
		if a > 0 && d > 0 {
			return a / (d / 1000)
		}
		return 0
	}
	best, bestW := 0.0, 0.0
	for _, w := range in.Weights {
		if w.PenaltyOnly || w.Weight <= bestW {
			continue
		}
		if v, ok := vals[w.AttributeID]; ok && v != 0 {
			best, bestW = math.Abs(v), w.Weight
		}
	}
	return best
}

// abyssalRolledWeights is a family's weights cut to the
// attributes one mutaplasmid moves. A family's weights cover
// every class in its SDE group (an afterburner shares
// propulsion's MWD signature weight, each smart bomb class rolls
// one damage type of four), and a weight its mutaplasmid never
// rolls must not choose the primary stat or the size figure.
func abyssalRolledWeights(weights []db.AbyssalFamilyWeight, ranges map[int64]abyssalRange) []db.AbyssalFamilyWeight {
	out := make([]db.AbyssalFamilyWeight, 0, len(weights))
	for _, w := range weights {
		if _, ok := ranges[w.AttributeID]; ok {
			out = append(out, w)
		}
	}
	return out
}

// abyssalDroneBandwidthAttr is droneBandwidthUsed: a drone's size
// class itself (light 5, medium 10, heavy 25, ...).
const abyssalDroneBandwidthAttr = 1272

// abyssalSameSize reports whether a rung belongs to the item's
// size class: its primary figure within a band of the source
// module's own. Drones are sized by bandwidth instead, exactly:
// their primary figure (the damage multiplier) is about the same
// for a light drone and a heavy one, and one SDE group holds
// every size.
func abyssalSameSize(in abyssalScoringInputs, rung map[int64]float64) bool {
	if bw, ok := in.Base[abyssalDroneBandwidthAttr]; ok && bw > 0 {
		return rung[abyssalDroneBandwidthAttr] == bw
	}
	src := abyssalPrimaryFigure(in, in.Base)
	r := abyssalPrimaryFigure(in, rung)
	if src <= 0 || r <= 0 {
		return false
	}
	ratio := r / src
	return ratio >= abyssalRungRatioMin && ratio <= abyssalRungRatioMax
}

// abyssalLadderPrice prices an index on a ladder: the log price
// and its log-space uncertainty, the rungs either side, and
// where the index fell. ok is false with no priced rung.
func abyssalLadderPrice(index float64, rungs []abyssalRung) (logV, sd float64, lo, hi int64, scope string, ok bool) {
	rs := make([]abyssalRung, 0, len(rungs))
	for _, r := range rungs {
		if r.Price > 0 {
			rs = append(rs, r)
		}
	}
	if len(rs) == 0 {
		return 0, 0, 0, 0, "", false
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Index != rs[j].Index {
			return rs[i].Index < rs[j].Index
		}
		return rs[i].Price < rs[j].Price
	})
	// A better module never prices lower: pool adjacent
	// violators over log price in index order.
	logs := make([]float64, len(rs))
	ones := make([]float64, len(rs))
	for i, r := range rs {
		logs[i], ones[i] = math.Log(r.Price), 1
	}
	mono := abyssalIsotonic(logs, ones)
	// Below the bottom rung the price falls along the end step,
	// never past abyssalBelowMaxDrop. Above the top rung it does
	// not climb at all: with nothing above to interpolate toward,
	// the top rung is the price (recreation cost and the learned
	// correction are what can still lift it).
	drop := func(step, d float64) float64 { return math.Min(step*d, abyssalBelowMaxDrop) }
	if len(rs) == 1 {
		d := index - rs[0].Index
		if d >= 0 {
			return mono[0], 0.3, rs[0].TypeID, 0, "single", true
		}
		dr := drop(abyssalLadderStep, -d)
		return mono[0] - dr, 0.3 + 0.5*dr, 0, rs[0].TypeID, "single", true
	}
	stepAt := func(i, j int) float64 {
		di := rs[j].Index - rs[i].Index
		if di <= 1e-9 {
			return abyssalLadderStep
		}
		s := (mono[j] - mono[i]) / di
		// A flat end still climbs; near-equal indices never
		// make a vertical one.
		return math.Min(math.Max(s, 0.25*abyssalLadderStep), 4*abyssalLadderStep)
	}
	last := len(rs) - 1
	switch {
	case index <= rs[0].Index:
		dr := drop(stepAt(0, 1), rs[0].Index-index)
		return mono[0] - dr, 0.12 + 0.6*dr, 0, rs[0].TypeID, "below", true
	case index >= rs[last].Index:
		return mono[last], abyssalAboveSD, rs[last].TypeID, 0, "above", true
	}
	for i := 1; i <= last; i++ {
		if index <= rs[i].Index {
			a, b := i-1, i
			span := rs[b].Index - rs[a].Index
			f := 0.0
			if span > 1e-9 {
				f = (index - rs[a].Index) / span
			}
			logV = mono[a] + (mono[b]-mono[a])*f
			gap := mono[b] - mono[a]
			// Tight beside a rung, widest midway across a gap.
			sd = 0.08 + 0.5*gap*2*math.Min(f, 1-f)
			return logV, sd, rs[a].TypeID, rs[b].TypeID, "between", true
		}
	}
	return mono[last], abyssalAboveSD, rs[last].TypeID, 0, "above", true
}

// abyssalSaneLadder reads a stored ladder figure for the corpus:
// a non-finite or runaway one (stored before the top-rung clamp,
// and rewritten by its next rescore) is missing, 0, so neither
// the market factor nor the learned correction ever trains on it.
func abyssalSaneLadder(v sql.NullFloat64) float64 {
	if !v.Valid || math.IsNaN(v.Float64) || math.IsInf(v.Float64, 0) ||
		v.Float64 <= 0 || v.Float64 > abyssalLadderSaneMax {
		return 0
	}
	return v.Float64
}

// abyssalIsotonic pools adjacent violators: the weighted
// non-decreasing sequence closest to vals.
func abyssalIsotonic(vals, weights []float64) []float64 {
	type block struct {
		v, w float64
		n    int
	}
	var st []block
	for i := range vals {
		st = append(st, block{vals[i], weights[i], 1})
		for len(st) > 1 && st[len(st)-2].v > st[len(st)-1].v {
			a, b := st[len(st)-2], st[len(st)-1]
			w := a.w + b.w
			st = append(st[:len(st)-2], block{(a.v*a.w + b.v*b.w) / w, w, a.n + b.n})
		}
	}
	out := make([]float64, 0, len(vals))
	for _, b := range st {
		for k := 0; k < b.n; k++ {
			out = append(out, b.v)
		}
	}
	return out
}

// abyssalMarketCalibration is the family's bounded market
// factor: the weighted median of clean listings' asks over their
// own ladder figures, in log space, shrunk toward no correction
// until abyssalCalibPrior effective listings have spoken, and
// clamped to [abyssalCalibMin, abyssalCalibMax]. A median cannot
// be dragged by a few bait or joke asks, and the clamp means no
// amount of listing can move a figure more than a quarter. Also
// returns the effective listings and the factor's log-space
// uncertainty.
func abyssalMarketCalibration(comps []abyssalComp) (factor, nEff, sd float64) {
	var logs []weightedValue
	sw, sw2 := 0.0, 0.0
	for _, c := range comps {
		if c.Ask <= 0 || c.LadderValue <= 0 {
			continue
		}
		w := c.DynamicsWeight
		if w <= 0 {
			w = 1
		}
		logs = append(logs, weightedValue{v: math.Log(c.Ask / c.LadderValue), w: w})
		sw += w
		sw2 += w * w
	}
	if len(logs) == 0 || sw2 <= 0 {
		return 1, 0, 0.2
	}
	shrunk, nEff := abyssalShrunkLogFactor(logs)
	return math.Exp(shrunk), nEff, 0.2 / math.Sqrt(1+nEff/4)
}

// abyssalShrunkLogFactor is the factor's arithmetic over weighted
// log ratios: the weighted median, shrunk by the effective count,
// clamped to the factor's bounds. Shared with the backtest, which
// judges the learned correction against exactly this.
func abyssalShrunkLogFactor(logs []weightedValue) (logFactor, nEff float64) {
	sw, sw2 := 0.0, 0.0
	for _, l := range logs {
		sw += l.w
		sw2 += l.w * l.w
	}
	if len(logs) == 0 || sw2 <= 0 {
		return 0, 0
	}
	nEff = sw * sw / sw2
	med := abyssalWeightedQuantile(logs, 0.5)
	shrunk := med * nEff / (nEff + abyssalCalibPrior)
	return math.Min(math.Max(shrunk, math.Log(abyssalCalibMin)), math.Log(abyssalCalibMax)), nEff
}

// abyssalCurveCorrection fits the bounded per-family
// correction for the curve fallback: the weighted median ratio
// of observed asks to what the curve predicts for each comp's
// own score and source price, clamped to [0.7, 1.4]. Returns the
// correction and how many comps carried a usable ratio.
func abyssalCurveCorrection(comps []abyssalComp) (float64, int) {
	var ratios []weightedValue
	for _, c := range comps {
		if c.Ask <= 0 || c.SourceTypical <= 0 {
			continue
		}
		pred := c.SourceTypical * abyssalCurveMult(c.Score)
		if pred <= 0 {
			continue
		}
		w := c.DynamicsWeight
		if w <= 0 {
			w = 1
		}
		ratios = append(ratios, weightedValue{v: c.Ask / pred, w: w})
	}
	if len(ratios) == 0 {
		return 1, 0
	}
	corr := abyssalWeightedQuantile(ratios, 0.5)
	return math.Min(math.Max(corr, 0.7), 1.4), len(ratios)
}

// estimateAbyssal is the pure estimator. Anchors in order: the
// item's place on its family's ladder of ordinary modules, then
// the score curve over the source module's price, then the
// source price alone as a labeled prior. No anchor, no estimate.
func estimateAbyssal(in abyssalEstimateInput) abyssalEstimate {
	est := abyssalEstimate{PremiumMult: 1, Calib: 1, MarketBasis: "factor"}
	usable := make([]abyssalComp, 0, len(in.Comps))
	for _, c := range in.Comps {
		if c.Ask > 0 {
			usable = append(usable, c)
		}
	}

	if in.HasIndex {
		if logV, sd, lo, hi, scope, ok := abyssalLadderPrice(in.Index, in.Rungs); ok {
			factor, _, sdCal := abyssalMarketCalibration(usable)
			if in.Learned > 0 {
				// The learned correction replaces the factor; its
				// uncertainty is its measured error (the floor below).
				factor, sdCal = in.Learned, 0
				est.MarketBasis = "learned"
			}
			ladder := math.Exp(logV)
			value := ladder * factor
			total := math.Sqrt(sd*sd + sdCal*sdCal)
			// The middle half is never narrower than the median
			// error the family's backtest measured.
			half := math.Max(abyssalQuartileZ*total, in.ErrFloor)
			low, high := value*math.Exp(-half), value*math.Exp(half)
			// Nobody needs to pay more than making one.
			if in.Recreation > 0 {
				if value > in.Recreation {
					value = in.Recreation
				}
				if high > in.Recreation {
					high = math.Max(in.Recreation, value)
				}
				if low > value {
					low = value
				}
			}
			priced := 0
			for _, r := range in.Rungs {
				if r.Price > 0 && r.TypeID != abyssalPerfectRungID {
					priced++
				}
			}
			est.OK = true
			est.Value, est.Low, est.High = value, low, high
			est.Basis, est.Scope = "ladder", scope
			est.Comps = priced
			est.LadderValue = ladder
			est.LadderLo, est.LadderHi = lo, hi
			est.Calib = factor
			return est
		}
	}

	// Curve / prior: needs the source module's own price.
	if in.SourceTypical <= 0 {
		return est
	}
	corr, n := abyssalCurveCorrection(usable)
	value := in.SourceTypical * abyssalCurveMult(in.Score) * corr
	est.OK = true
	est.Value = value
	if n > 0 {
		est.Basis = "curve"
		est.Comps = n
		est.Low, est.High = value*0.65, value*1.55
	} else {
		est.Basis = "prior"
		est.Low, est.High = value*0.6, value*1.7
	}
	return est
}

// abyssalRecreationPath is one way to roll another item: a base
// module and a mutaplasmid grade that produces the item's type,
// with our prices for both, the base's attributes and the
// grade's bands.
type abyssalRecreationPath struct {
	SourceTypeID   int64
	BaseTypical    float64
	Base           map[int64]float64
	MutatorTypeID  int64
	MutatorTypical float64
	Ranges         map[int64]abyssalRange
}

// Failed rolls are not worthless: each one is a module someone
// will buy. Their resale is credited against the next attempt,
// at a haircut (selling a pile of mediocre rolls is slow and
// discounted), and never for more than a share of an attempt's
// cost, so making one always costs at least a quarter of what
// the bare attempts would.
const (
	abyssalResidualHaircut  = 0.5
	abyssalResidualMaxShare = 0.75
)

// abyssalRecreationCost prices rolling another item at least
// this good along the cheapest path: for every base and
// mutaplasmid grade that can reach the item's primary rolls,
// the expected attempts on that path's own odds (the scorer's
// survival measure with the item as the target, placed in that
// base's bands), each attempt costing the base plus the
// mutaplasmid less the resale of the roll that missed. residual
// prices a typical roll on a path (0 when unknown); nil credits
// nothing. ok is false when no priced, reachable path exists;
// attempts is the cheapest path's.
func abyssalRecreationCost(sc abyssalScoring, in abyssalScoringInputs, target map[int64]float64, paths []abyssalRecreationPath, residual func(abyssalRecreationPath) float64) (cost, attempts float64, ok bool) {
	if !sc.HasScore {
		return 0, 0, false
	}
	var primaries []int64
	for _, a := range sc.Attrs {
		if a.Scored && a.Primary {
			primaries = append(primaries, a.AttributeID)
		}
	}
	best := math.Inf(1)
	for _, p := range paths {
		if p.BaseTypical <= 0 || p.MutatorTypical <= 0 || p.Base == nil || len(p.Ranges) == 0 {
			continue
		}
		reachable := true
		for _, id := range primaries {
			rng, okR := p.Ranges[id]
			base, okB := p.Base[id]
			want, okW := target[id]
			if !okR || !okB || !okW || base == 0 {
				reachable = false
				break
			}
			high := true
			if hg, ok := in.HighGood[id]; ok {
				high = hg
			}
			if (high && base*rng.MaxMult < want) || (!high && base*rng.MinMult > want) {
				reachable = false
				break
			}
		}
		if !reachable {
			continue
		}
		odds := scoreAbyssal(abyssalScoringInputs{
			Family: in.Family, Rolls: target, Ranges: p.Ranges,
			Base: p.Base, HighGood: in.HighGood, Weights: in.Weights,
		})
		n := odds.Attempts
		if !odds.HasScore || n <= 0 {
			continue
		}
		try := p.BaseTypical + p.MutatorTypical
		credit := 0.0
		if residual != nil {
			credit = math.Min(abyssalResidualHaircut*residual(p), abyssalResidualMaxShare*try)
			credit = math.Max(credit, 0)
		}
		if c := try + (n-1)*(try-credit); c < best {
			best, attempts = c, n
		}
	}
	if math.IsInf(best, 1) {
		return 0, 0, false
	}
	return best, attempts, true
}

// abyssalPerfectRungID marks the ladder's perfect-roll rung: not
// a module anyone lists, the best roll the item's mutaplasmid can
// make on its source base.
const abyssalPerfectRungID = -1

// abyssalPerfectRoll is the best roll the item's bands allow on
// its source base: every moved attribute at its band's good end.
func abyssalPerfectRoll(in abyssalScoringInputs) map[int64]float64 {
	vals := make(map[int64]float64, len(in.Base))
	for id, v := range in.Base {
		vals[id] = v
	}
	for id, r := range in.Ranges {
		b, ok := in.Base[id]
		if !ok {
			continue
		}
		high := true
		if hg, ok := in.HighGood[id]; ok {
			high = hg
		}
		if high {
			vals[id] = b * r.MaxMult
		} else {
			vals[id] = b * r.MinMult
		}
	}
	return vals
}

// abyssalPerfectRung is the ladder's fixed point past the top
// rung: the perfect roll, at its own index, priced at what making
// one would cost (the same recreation paths and resale credit as
// the item's own cap). An item past the top rung then prices
// between that rung and this one instead of stopping at the top:
// better rolls price higher, and nothing can pass what a perfect
// roll costs to make. ok is false when the perfect roll does not
// sit above the top rung, cannot be priced, or would price below
// it (the top rung is then the ceiling).
func abyssalPerfectRung(in abyssalScoringInputs, rungs []abyssalRung, paths []abyssalRecreationPath, residual func(abyssalRecreationPath) float64) (abyssalRung, bool) {
	// The ladder's top: its highest index, and its dearest price
	// (the isotonic top end is never dearer than that).
	topIdx, topPrice := math.Inf(-1), 0.0
	for _, r := range rungs {
		if r.Price > 0 {
			topIdx = math.Max(topIdx, r.Index)
			topPrice = math.Max(topPrice, r.Price)
		}
	}
	if math.IsInf(topIdx, -1) {
		return abyssalRung{}, false
	}
	perfect := abyssalPerfectRoll(in)
	idx, ok := abyssalLadderIndex(in, perfect)
	if !ok || idx <= topIdx+1e-6 {
		return abyssalRung{}, false
	}
	sc := scoreAbyssal(abyssalScoringInputs{
		Family: in.Family, Rolls: perfect, Ranges: in.Ranges,
		Base: in.Base, HighGood: in.HighGood, Weights: in.Weights,
	})
	cost, _, ok := abyssalRecreationCost(sc, in, perfect, paths, residual)
	if !ok || cost <= topPrice {
		return abyssalRung{}, false
	}
	return abyssalRung{TypeID: abyssalPerfectRungID, Price: cost, Index: idx}, true
}

// abyssalTypicalRoll is a path's middle roll: every attribute
// the grade moves at the centre of its band on that base.
func abyssalTypicalRoll(p abyssalRecreationPath) map[int64]float64 {
	vals := make(map[int64]float64, len(p.Base))
	for id, v := range p.Base {
		vals[id] = v
	}
	for id, r := range p.Ranges {
		if b, ok := p.Base[id]; ok {
			vals[id] = b * (r.MinMult + r.MaxMult) / 2
		}
	}
	return vals
}

// ---------------------------------------------------------------------
// The worker pass: score and estimate the due slice, store
// everything, and mark the holding contracts for repricing.
// ---------------------------------------------------------------------

// abyssalFamilyKey normalizes a family or group name for
// matching: the seeded family names are slugs of SDE group
// names, and matching with underscores removed absorbs the
// groups whose names slug ambiguously.
func abyssalFamilyKey(s string) string {
	return strings.ReplaceAll(abyssalSlug(s), "_", "")
}

// abyssalSlug turns an SDE group name into the seeded
// family slug shape ("Warp Scrambler" -> "warp_scrambler").
func abyssalSlug(s string) string {
	var b strings.Builder
	lastUnderscore := true // trims a leading separator too
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

// abyssalSoldWeight is the say of a confirmed sale: ESI
// answered that the contract was accepted (pub_contracts_sales.go),
// so its price is what a buyer paid, not an ask.
const abyssalSoldWeight = 2.5

// abyssalSightingWeight: exit dynamics over ask levels. A
// confirmed sale counts most; a listing still live counts
// once; one that vanished before its term without a confirmed
// sale counts a little more (deleted, or accepted too long ago
// for ESI to say); one that expired counts least (it sat the
// whole term at that ask).
func abyssalSightingWeight(departed bool, departKind string, sold bool) float64 {
	if sold {
		return abyssalSoldWeight
	}
	if !departed {
		return 1.0
	}
	if departKind == "vanished" {
		return 1.25
	}
	return 0.6
}

// abyssalAttribution is what a sighting's contract held
// besides the item (schema 069), stamped while it was stored.
type abyssalAttribution struct {
	Price         float64
	Stamped       bool // false: stamped before schema 069, contract gone
	AbyssalLines  int64
	WantedLines   int64
	OtherLines    int64
	OtherUnpriced int64
	OtherValue    float64
}

// abyssalUnstampedWeight is the say of a sighting whose
// contract mix was never recorded: it may be a bundle.
const abyssalUnstampedWeight = 0.35

// abyssalAttributedAsk is the price a sighting says the item
// itself asked, and the say that attribution earns. A contract
// is the item's own ask when the item is all it offers. Other
// offered lines are subtracted at their appraised value when
// every one is priced and together they are a small part of the
// price (a bundle priced mostly by its passengers says little
// about the item). Two or more abyssal items, or anything
// wanted in exchange, cannot be split without guessing -- or
// without pricing items by our own estimates, which would feed
// the model its own output -- so those sightings are left out.
func abyssalAttributedAsk(a abyssalAttribution) (ask, weight float64, ok bool) {
	if a.Price <= 0 {
		return 0, 0, false
	}
	if !a.Stamped {
		return a.Price, abyssalUnstampedWeight, true
	}
	if a.AbyssalLines > 1 || a.WantedLines > 0 {
		return 0, 0, false
	}
	if a.OtherLines == 0 {
		return a.Price, 1, true
	}
	if a.OtherUnpriced > 0 || a.OtherValue > 0.25*a.Price {
		return 0, 0, false
	}
	// The subtraction is an estimate itself; the residual
	// speaks a little more quietly the bigger it was.
	return a.Price - a.OtherValue, 1 - a.OtherValue/a.Price, true
}

// abyssalAgeHalfLife: a sighting's say halves every 30 days
// since it was last on the market, so the corpus follows the
// market's drift while a thin family keeps its history.
const abyssalAgeHalfLife = 30 * 24 * time.Hour

// abyssalAgeWeight is a sighting's recency and staleness: its
// say decays with time since it was last seen (or left), never
// below a tenth; a listing still live after more than ten days
// is evidence its ask is above what clears, and loses say as it
// sits (to a floor of 0.35 at two months).
func abyssalAgeWeight(departed sql.NullTime, firstSeen, lastSeen, now time.Time) float64 {
	ref := lastSeen
	if departed.Valid {
		ref = departed.Time
	}
	age := now.Sub(ref)
	if age < 0 {
		age = 0
	}
	w := math.Max(0.1, math.Pow(0.5, float64(age)/float64(abyssalAgeHalfLife)))
	if !departed.Valid {
		sat := lastSeen.Sub(firstSeen).Hours() / 24
		if sat > 10 {
			w *= math.Max(0.35, 1-(sat-10)/50)
		}
	}
	return w
}

type abyssalRollKV struct {
	AttributeID int64   `json:"attribute_id"`
	Value       float64 `json:"value"`
}

func (app *Application) scoreAbyssalItems(ctx context.Context) int {
	q := app.queries
	now := time.Now().UTC()
	due, err := q.ListAbyssalItemsDueScoring(ctx, db.ListAbyssalItemsDueScoringParams{
		Cutoff:  now.Add(-abyssalRescoreAge),
		MaxRows: abyssalScoreBatch,
	})
	if err != nil {
		logging.Errorf("worker: abyssal scoring: due list: %v", err)
		return 0
	}
	if len(due) == 0 {
		return 0
	}

	// Family weights, keyed canonically and by loose key.
	weightRows, err := q.ListAllAbyssalFamilyWeights(ctx)
	if err != nil {
		logging.Errorf("worker: abyssal scoring: weights: %v", err)
		return 0
	}
	weightsByFamily := map[string][]db.AbyssalFamilyWeight{}
	canonByKey := map[string]string{}
	for _, w := range weightRows {
		weightsByFamily[w.Family] = append(weightsByFamily[w.Family], w)
		canonByKey[abyssalFamilyKey(w.Family)] = w.Family
	}

	// Each due item's family: its mutated type's SDE group,
	// slugged, canonicalized against the seeded families.
	typeIDs := make([]int64, 0, len(due))
	for _, it := range due {
		typeIDs = append(typeIDs, it.TypeID)
	}
	groupByType := map[int64]string{}
	if rows, err := q.ListTypeGroupNames(ctx, typeIDs); err == nil {
		for _, r := range rows {
			groupByType[r.TypeID] = r.Name
		}
	}
	familyOf := func(typeID int64) string {
		slug := abyssalSlug(groupByType[typeID])
		if canon, ok := canonByKey[abyssalFamilyKey(slug)]; ok {
			return canon
		}
		return slug
	}

	// Ranges per mutator, base attributes per source type,
	// attribute directions: cached across the batch.
	rangesByMutator := map[int64]map[int64]abyssalRange{}
	rangesFor := func(mutator int64) map[int64]abyssalRange {
		if m, ok := rangesByMutator[mutator]; ok {
			return m
		}
		m := map[int64]abyssalRange{}
		if rows, err := q.ListAbyssalRangesForMutator(ctx, mutator); err == nil {
			for _, r := range rows {
				m[r.AttributeID] = abyssalRange{MinMult: r.MinMult, MaxMult: r.MaxMult}
			}
		}
		rangesByMutator[mutator] = m
		return m
	}
	baseBySource := map[int64]map[int64]float64{}
	baseFor := func(source int64) map[int64]float64 {
		if m, ok := baseBySource[source]; ok {
			return m
		}
		m := map[int64]float64{}
		if rows, err := q.ListSDETypeAttributes(ctx, source); err == nil {
			for _, r := range rows {
				m[r.AttributeID] = r.Value
			}
		}
		baseBySource[source] = m
		return m
	}
	// Every mutaplasmid grade seen producing a mutated type: the
	// recreation paths beyond the grade this item was rolled with.
	mutatorsByType := map[int64][]int64{}
	mutatorsFor := func(typeID, own int64) []int64 {
		ms, ok := mutatorsByType[typeID]
		if !ok {
			if rows, err := q.ListAbyssalMutatorsForType(ctx, typeID); err == nil {
				ms = rows
			}
			mutatorsByType[typeID] = ms
		}
		for _, m := range ms {
			if m == own {
				return ms
			}
		}
		return append([]int64{own}, ms...)
	}
	sourcesByMutator := map[int64][]int64{}
	sourcesFor := func(mutator int64) []int64 {
		if s, ok := sourcesByMutator[mutator]; ok {
			return s
		}
		var s []int64
		if rows, err := q.ListAbyssalSourcesForMutator(ctx, mutator); err == nil {
			s = rows
		}
		sourcesByMutator[mutator] = s
		return s
	}

	// The calibration corpora of the batch's families,
	// selected by SDE group name (the family's identity)
	// rather than the stored family column, which this very
	// pass is responsible for filling.
	corpusByFamily := map[string][]db.ListAbyssalCorpusByGroupNamesRow{}
	{
		groupNames := map[string]bool{}
		for _, name := range groupByType {
			if name != "" {
				groupNames[name] = true
			}
		}
		names := make([]string, 0, len(groupNames))
		for n := range groupNames {
			names = append(names, n)
		}
		if len(names) > 0 {
			rows, err := q.ListAbyssalCorpusByGroupNames(ctx, names)
			if err != nil {
				logging.Errorf("worker: abyssal scoring: corpus: %v", err)
			}
			for _, r := range rows {
				slug := abyssalSlug(r.GroupName)
				fam := slug
				if canon, ok := canonByKey[abyssalFamilyKey(slug)]; ok {
					fam = canon
				}
				corpusByFamily[fam] = append(corpusByFamily[fam], r)
			}
		}
	}

	// Attribute directions across everything the batch can
	// touch.
	attrIDs := map[int64]bool{}
	for _, w := range weightRows {
		attrIDs[w.AttributeID] = true
	}
	for _, it := range due {
		for attr := range rangesFor(it.MutatorTypeID) {
			attrIDs[attr] = true
		}
	}
	highGood := map[int64]bool{}
	if len(attrIDs) > 0 {
		ids := make([]int64, 0, len(attrIDs))
		for id := range attrIDs {
			ids = append(ids, id)
		}
		if rows, err := q.ListSDEAttributeTypesByIDs(ctx, ids); err == nil {
			for _, r := range rows {
				highGood[r.AttributeID] = r.HighIsGood != 0
			}
		}
	}

	// Each due source module's ladder: the ordinary modules of
	// its SDE group, with their base stats.
	ladderBySource := map[int64][]int64{}
	{
		srcSet := map[int64]bool{}
		for _, it := range due {
			if it.SourceTypeID > 0 {
				srcSet[it.SourceTypeID] = true
			}
		}
		ids := make([]int64, 0, len(srcSet))
		for id := range srcSet {
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			rows, err := q.ListAbyssalLadderTypes(ctx, ids)
			if err != nil {
				logging.Errorf("worker: abyssal scoring: ladder: %v", err)
			}
			for _, r := range rows {
				ladderBySource[r.SourceTypeID] = append(ladderBySource[r.SourceTypeID], r.TypeID)
				baseFor(r.TypeID)
			}
		}
	}

	// Prices for everything the math can anchor on: due
	// items' sources and mutators, their ladders' rungs,
	// recreation candidate bases, and corpus comps' sources
	// (the curve correction's denominators). One
	// AppraiseLines call, house order.
	priceTypes := map[int64]bool{}
	for _, rungs := range ladderBySource {
		for _, t := range rungs {
			priceTypes[t] = true
		}
	}
	for _, it := range due {
		priceTypes[it.SourceTypeID] = true
		for _, m := range mutatorsFor(it.TypeID, it.MutatorTypeID) {
			priceTypes[m] = true
			rangesFor(m)
			for _, src := range sourcesFor(m) {
				priceTypes[src] = true
				baseFor(src)
			}
		}
	}
	for _, rows := range corpusByFamily {
		for _, r := range rows {
			priceTypes[r.SourceTypeID] = true
		}
	}
	priceByType := map[int64]float64{}
	{
		lines := make([]module.AppraisalLine, 0, len(priceTypes))
		for id := range priceTypes {
			if id > 0 {
				lines = append(lines, module.AppraisalLine{TypeID: id, Quantity: 1, Included: true})
			}
		}
		if len(lines) > 0 {
			if priced, err := app.AppraiseLines(ctx, lines); err == nil {
				for _, l := range priced.Lines {
					if l.Priced {
						priceByType[l.TypeID] = l.UnitPrice
					}
				}
			} else {
				logging.Errorf("worker: abyssal scoring: prices: %v", err)
			}
		}
	}

	// Phase one: score every due item in memory, so the
	// corpus an item is priced against already carries this
	// batch's fresh scores rather than last cycle's.
	type scoredItem struct {
		it     db.ListAbyssalItemsDueScoringRow
		family string
		rolls  map[int64]float64
		in     abyssalScoringInputs
		sc     abyssalScoring
	}
	entries := make([]scoredItem, 0, len(due))
	batchScores := map[int64]float64{}
	for _, it := range due {
		family := familyOf(it.TypeID)
		var rollList []abyssalRollKV
		if err := json.Unmarshal(it.Attributes, &rollList); err != nil {
			continue
		}
		rolls := make(map[int64]float64, len(rollList))
		for _, kv := range rollList {
			rolls[kv.AttributeID] = kv.Value
		}
		ranges := rangesFor(it.MutatorTypeID)
		in := abyssalScoringInputs{
			Family:   family,
			Rolls:    rolls,
			Ranges:   ranges,
			Base:     baseFor(it.SourceTypeID),
			HighGood: highGood,
			Weights:  abyssalRolledWeights(weightsByFamily[family], ranges),
		}
		sc := scoreAbyssal(in)
		entries = append(entries, scoredItem{it: it, family: family, rolls: rolls, in: in, sc: sc})
		if sc.HasScore {
			batchScores[it.ItemID] = sc.Score
		}
	}

	// How many of each family's comps each seller lists.
	sellerCount := map[string]map[int64]int{}
	for fam, rows := range corpusByFamily {
		m := map[int64]int{}
		for _, r := range rows {
			if r.IssuerID.Valid {
				m[r.IssuerID.Int64]++
			}
		}
		sellerCount[fam] = m
	}

	compsFor := func(family string, selfID int64) []abyssalComp {
		rows := corpusByFamily[family]
		comps := make([]abyssalComp, 0, len(rows))
		for _, r := range rows {
			if r.ItemID == selfID {
				continue // an item's own ask never prices the item
			}
			score, ok := batchScores[r.ItemID]
			if !ok {
				if !r.Score.Valid {
					continue
				}
				score = r.Score.Float64
			}
			ask, attrW, ok := abyssalAttributedAsk(abyssalAttribution{
				Price:         r.Price,
				Stamped:       r.AbyssalLines.Valid,
				AbyssalLines:  r.AbyssalLines.Int64,
				WantedLines:   r.WantedLines.Int64,
				OtherLines:    r.OtherLines.Int64,
				OtherUnpriced: r.OtherUnpriced.Int64,
				OtherValue:    r.OtherValue.Float64,
			})
			if !ok {
				continue // the contract cannot say what this item asked
			}
			w := abyssalSightingWeight(r.DepartedAt.Valid, r.DepartKind.String, r.SoldAt.Valid) * attrW
			w *= abyssalAgeWeight(r.DepartedAt, r.FirstSeenAt, r.LastSeenAt, now)
			// One seller's listings share one voice that grows
			// only with the square root of how many they list.
			if r.IssuerID.Valid {
				if n := sellerCount[family][r.IssuerID.Int64]; n > 1 {
					w /= math.Sqrt(float64(n))
				}
			}
			// A listing hugging our own stored estimate may
			// be anchored on it; halve its say.
			if r.EstValue.Valid && r.EstValue.Float64 > 0 {
				if d := math.Abs(ask-r.EstValue.Float64) / r.EstValue.Float64; d < 0.02 {
					w *= 0.5
				}
			}
			comps = append(comps, abyssalComp{
				ItemID:         r.ItemID,
				SourceTypeID:   r.SourceTypeID,
				Score:          score,
				Ask:            ask,
				SourceTypical:  priceByType[r.SourceTypeID],
				LadderValue:    abyssalSaneLadder(r.LadderValue),
				DynamicsWeight: w,
				PrimaryAbs:     r.PrimaryAbs.Float64,
				Rolls:          r.Attributes,
				MutatorTypeID:  r.MutatorTypeID,
				FirstSeen:      r.FirstSeenAt,
				Sold:           r.SoldAt.Valid,
			})
		}
		return comps
	}

	// Each family's learned correction, fitted once per pass
	// from the same clean comps, and its backtest stored.
	learnedByFamily := map[string]*abyssalLearned{}
	learnedFor := func(family string) *abyssalLearned {
		if l, ok := learnedByFamily[family]; ok {
			return l
		}
		weights := weightsByFamily[family]
		var rows []abyssalLearnRow
		if len(weights) > 0 {
			for _, c := range compsFor(family, 0) {
				if c.LadderValue <= 0 || c.Ask <= 0 {
					continue
				}
				var kv []abyssalRollKV
				if json.Unmarshal(c.Rolls, &kv) != nil {
					continue
				}
				rolls := make(map[int64]float64, len(kv))
				for _, a := range kv {
					rolls[a.AttributeID] = a.Value
				}
				rows = append(rows, abyssalLearnRow{
					ItemID: c.ItemID,
					X:      abyssalLearnFeatures(weights, highGood, rangesFor(c.MutatorTypeID), rolls),
					Y:      math.Log(c.Ask / c.LadderValue),
					W:      c.DynamicsWeight,
					Seen:   c.FirstSeen,
					Sold:   c.Sold,
				})
			}
		}
		l := abyssalLearnFamily(rows)
		learnedByFamily[family] = l
		f := l.Fit
		nf := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: f.HasBacktest} }
		if err := q.UpsertAbyssalFamilyFit(ctx, db.UpsertAbyssalFamilyFitParams{
			Family: family, Comps: int64(f.Comps), NEff: f.NEff,
			HoldoutN: int64(f.HoldoutN), SalesN: int64(f.SalesN),
			MaeLadder: nf(f.MAELadder), MaeModel: nf(f.MAEModel), BiasModel: nf(f.BiasModel),
			UseModel: f.UseModel, FittedAt: now,
		}); err != nil {
			logging.Errorf("worker: abyssal scoring: store fit %s: %v", family, err)
		}
		return l
	}

	// Phase two: estimate and store.
	scored := 0
	touched := make([]int64, 0, len(entries))
	for _, e := range entries {
		it, family, rolls, in, sc := e.it, e.family, e.rolls, e.in, e.sc
		params := db.UpdateAbyssalItemEstimateParams{
			ItemID:         it.ItemID,
			Family:         family,
			PremiumMult:    1,
			CapAttributeID: sc.CapAttributeID,
			CapKind:        sc.CapKind,
			PrimaryAbs:     sql.NullFloat64{Float64: sc.PrimaryAbs, Valid: sc.HasScore && sc.PrimaryAbs > 0},
			MarketCalib:    1,
			MarketBasis:    "factor",
			ScoredAt:       now,
		}
		pcts := make(map[string]float64, len(sc.Percentiles))
		for id, p := range sc.Percentiles {
			pcts[strconv.FormatInt(id, 10)] = math.Round(p*10000) / 10000
		}
		params.Percentiles, _ = json.Marshal(pcts)
		if sc.Derived != nil {
			params.DerivedStats, _ = json.Marshal(sc.Derived)
		} else {
			params.DerivedStats = []byte("[]")
		}
		if params.Percentiles == nil {
			params.Percentiles = []byte("{}")
		}
		if sc.HasScore {
			params.Score = sql.NullFloat64{Float64: sc.Score, Valid: true}
			// The item's ladder: its family's ordinary modules
			// of its size class, each indexed exactly as the
			// item is, against the item's own base and bands.
			index, hasIndex := abyssalLadderIndex(in, rolls)
			var rungs []abyssalRung
			for _, t := range ladderBySource[it.SourceTypeID] {
				vals := baseFor(t)
				if !abyssalSameSize(in, vals) {
					continue
				}
				if ri, ok := abyssalLadderIndex(in, vals); ok {
					rungs = append(rungs, abyssalRung{TypeID: t, Price: priceByType[t], Index: ri})
				}
			}
			// Recreation caps the estimate: the cheapest path
			// over every base and mutaplasmid grade seen making
			// this type, each on its own odds, with the missed
			// rolls' resale priced on the same ladder.
			var paths []abyssalRecreationPath
			for _, m := range mutatorsFor(it.TypeID, it.MutatorTypeID) {
				for _, src := range sourcesFor(m) {
					paths = append(paths, abyssalRecreationPath{
						SourceTypeID: src, BaseTypical: priceByType[src], Base: baseFor(src),
						MutatorTypeID: m, MutatorTypical: priceByType[m], Ranges: rangesFor(m),
					})
				}
			}
			residual := func(p abyssalRecreationPath) float64 {
				idx, ok := abyssalLadderIndex(in, abyssalTypicalRoll(p))
				if !ok {
					return 0
				}
				if logV, _, _, _, _, ok := abyssalLadderPrice(idx, rungs); ok {
					return math.Exp(logV)
				}
				return 0
			}
			recreation := 0.0
			if cost, attempts, ok := abyssalRecreationCost(sc, in, rolls, paths, residual); ok {
				recreation = cost
				params.RecreationCost = sql.NullFloat64{Float64: cost, Valid: true}
				params.RecreationAttempts = sql.NullFloat64{Float64: attempts, Valid: true}
			}
			// The perfect roll closes the ladder from above, so a
			// roll past the top rung still prices by how far past.
			if pr, ok := abyssalPerfectRung(in, rungs, paths, residual); ok {
				rungs = append(rungs, pr)
			}
			// The family's learned correction, where its
			// backtest earned it a say; the measured error
			// floors the range either way.
			learned := learnedFor(family)
			corr, useLearned := learned.Correction(it.ItemID, abyssalLearnFeatures(weightsByFamily[family], highGood, in.Ranges, rolls))
			errFloor := 0.0
			if learned.Fit.HasBacktest {
				errFloor = learned.Fit.MAELadder
				if useLearned {
					errFloor = learned.Fit.MAEModel
				}
			}
			if !useLearned {
				corr = 0
			}
			est := estimateAbyssal(abyssalEstimateInput{
				Learned:       corr,
				ErrFloor:      errFloor,
				Score:         sc.Score,
				SourceTypical: priceByType[it.SourceTypeID],
				SourceTypeID:  it.SourceTypeID,
				PrimaryAbs:    sc.PrimaryAbs,
				Index:         index,
				HasIndex:      hasIndex,
				Rungs:         rungs,
				Recreation:    recreation,
				Comps:         compsFor(family, it.ItemID),
			})
			params.MarketCalib = 1
			if est.OK {
				params.EstValue = sql.NullFloat64{Float64: est.Value, Valid: true}
				params.EstLow = sql.NullFloat64{Float64: est.Low, Valid: true}
				params.EstHigh = sql.NullFloat64{Float64: est.High, Valid: true}
				params.EstBasis = est.Basis
				params.EstScope = est.Scope
				params.EstComps = int64(est.Comps)
				params.PremiumMult = est.PremiumMult
				params.MarketCalib = est.Calib
				params.MarketBasis = est.MarketBasis
				if est.LadderValue > 0 {
					params.LadderValue = sql.NullFloat64{Float64: est.LadderValue, Valid: true}
				}
				params.LadderLoTypeID, params.LadderHiTypeID = est.LadderLo, est.LadderHi
			}
		}
		if err := q.UpdateAbyssalItemEstimate(ctx, params); err != nil {
			logging.Errorf("worker: abyssal scoring: store %d: %v", it.ItemID, err)
			continue
		}
		scored++
		touched = append(touched, it.ItemID)
	}
	if len(touched) > 0 {
		if err := q.TouchPublicContractsForItemIDs(ctx, touched); err != nil {
			logging.Errorf("worker: abyssal scoring: touch contracts: %v", err)
		}
	}
	return scored
}
