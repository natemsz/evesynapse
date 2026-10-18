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
// The estimate is anchored on absolute performance, never on
// provenance. The comparable set is the family's corpus
// bucketed by score band: items are priced against listings
// with a matching performance profile, whatever module they
// were mutated from. A provenance premium is applied only
// when stat-matched corpus data actually measures one (the
// median ask of the item's own source module against the
// rest of the band, shrunk by sample size); unmeasured, it is
// one, exactly. When the bucket is too thin the estimate
// falls back to the source module's own price run through
// the score curve, with a bounded per-family correction
// fitted from whatever listings exist; the correction is the
// only place asking prices touch that path, and it is
// clamped. Calibration weights exit dynamics over ask
// levels: a listing that left before its term counts more
// than one still sitting, and one that sat out its full term
// counts least, because disappearance is never read as a
// sale. An item's own ask is never an input to its estimate:
// the corpus a score is priced against excludes the item
// itself, and a listing priced suspiciously close to our own
// stored estimate is down-weighted, because a seller may be
// anchoring on us.
//
// Recreation cost is stored beside the estimate as a
// cross-check, never blended into it: the expected attempts
// to roll at least this item's profile again, times the
// cheapest reachable base module plus the mutaplasmid.
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
	// abyssalCorpusMin is the bucket size under which a
	// family band is "too thin" and the curve speaks instead.
	abyssalCorpusMin = 5
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
// family, its most recent exchange listing's ask, the ask's
// source module price (for the curve correction), and the
// weight exit dynamics gave it. The item being estimated is
// never among its own comps: callers exclude it, and the
// contamination test holds that line.
type abyssalComp struct {
	ItemID         int64
	SourceTypeID   int64
	Score          float64
	Ask            float64
	SourceTypical  float64
	DynamicsWeight float64
	// PrimaryAbs is the comp's absolute primary figure; 0
	// when unknown (then no stat matching applies to it).
	PrimaryAbs float64
}

// abyssalEstimateInput is the estimator's whole world.
type abyssalEstimateInput struct {
	Score         float64
	SourceTypical float64 // the item's own source module price; 0 = none
	SourceTypeID  int64
	// PrimaryAbs is the item's absolute primary figure;
	// comps whose own figure sits outside a factor of 2.5 of
	// it are not comparables (a small shield booster's rolls
	// say nothing about an extra-large one's price).
	PrimaryAbs float64
	Comps      []abyssalComp // same family, the item itself excluded
}

// abyssalEstimate is one stored estimate.
type abyssalEstimate struct {
	OK          bool
	Value       float64
	Low         float64
	High        float64
	Basis       string // "corpus", "curve" or "prior"
	Scope       string // "module" or "family" for the corpus basis
	Comps       int    // listings the figure rests on
	PremiumMult float64
}

// abyssalBandCount is the fixed score-band grid the corpus
// calibration is fitted on. Band medians are monotonized, so
// the corpus path upholds the same fairness property as the
// curve: a better score never prices lower out of the same
// corpus.
const abyssalBandCount = 5

func abyssalBandOf(score float64) int {
	b := int(clamp01Abyssal(score) * abyssalBandCount)
	if b >= abyssalBandCount {
		b = abyssalBandCount - 1
	}
	return b
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

type abyssalBandStat struct {
	median, q1, q3 float64
	n              int
}

// abyssalBandStats buckets comps onto the band grid, fills
// empty bands from their neighbours (log-space interpolation,
// flat past the edges) and monotonizes the medians.
func abyssalBandStats(comps []abyssalComp) ([abyssalBandCount]abyssalBandStat, int) {
	var stats [abyssalBandCount]abyssalBandStat
	var per [abyssalBandCount][]weightedValue
	total := 0
	for _, c := range comps {
		if c.Ask <= 0 {
			continue
		}
		w := c.DynamicsWeight
		if w <= 0 {
			w = 1
		}
		b := abyssalBandOf(c.Score)
		per[b] = append(per[b], weightedValue{v: c.Ask, w: w})
		total++
	}
	filled := make([]bool, abyssalBandCount)
	for b := 0; b < abyssalBandCount; b++ {
		if len(per[b]) == 0 {
			continue
		}
		filled[b] = true
		stats[b].n = len(per[b])
		stats[b].median = abyssalWeightedQuantile(per[b], 0.5)
		stats[b].q1 = abyssalWeightedQuantile(per[b], 0.25)
		stats[b].q3 = abyssalWeightedQuantile(per[b], 0.75)
	}
	// Fill empty bands between (or beyond) filled ones.
	for b := 0; b < abyssalBandCount; b++ {
		if filled[b] {
			continue
		}
		lo, hi := -1, -1
		for k := b - 1; k >= 0; k-- {
			if filled[k] {
				lo = k
				break
			}
		}
		for k := b + 1; k < abyssalBandCount; k++ {
			if filled[k] {
				hi = k
				break
			}
		}
		switch {
		case lo >= 0 && hi >= 0:
			f := float64(b-lo) / float64(hi-lo)
			stats[b].median = math.Exp(math.Log(stats[lo].median)*(1-f) + math.Log(stats[hi].median)*f)
		case lo >= 0:
			stats[b].median = stats[lo].median
		case hi >= 0:
			stats[b].median = stats[hi].median
		}
		stats[b].q1 = stats[b].median * 0.75
		stats[b].q3 = stats[b].median * 1.35
	}
	// Monotonize: a higher band never prices below a lower
	// one out of the same corpus.
	for b := 1; b < abyssalBandCount; b++ {
		if stats[b].median < stats[b-1].median {
			stats[b].median = stats[b-1].median
		}
		if stats[b].q1 > stats[b].median {
			stats[b].q1 = stats[b].median
		}
		if stats[b].q3 < stats[b].median {
			stats[b].q3 = stats[b].median
		}
	}
	return stats, total
}

// abyssalProvenancePremium measures, inside the item's own
// band, whether the item's source module trades above or
// below the rest of the band at matched performance. It
// returns 1 (no premium) unless both sides of the comparison
// have at least two listings; a measured ratio is shrunk
// toward 1 by sample size and clamped.
func abyssalProvenancePremium(in abyssalEstimateInput) float64 {
	band := abyssalBandOf(in.Score)
	var same, other []weightedValue
	for _, c := range in.Comps {
		if c.Ask <= 0 || abyssalBandOf(c.Score) != band {
			continue
		}
		w := c.DynamicsWeight
		if w <= 0 {
			w = 1
		}
		if c.SourceTypeID == in.SourceTypeID && in.SourceTypeID > 0 {
			same = append(same, weightedValue{v: c.Ask, w: w})
		} else {
			other = append(other, weightedValue{v: c.Ask, w: w})
		}
	}
	if len(same) < 2 || len(other) < 2 {
		return 1
	}
	raw := abyssalWeightedQuantile(same, 0.5) / abyssalWeightedQuantile(other, 0.5)
	n := len(same)
	if len(other) < n {
		n = len(other)
	}
	premium := 1 + (raw-1)*float64(n)/(float64(n)+6)
	if premium < 0.85 {
		premium = 0.85
	}
	if premium > 1.4 {
		premium = 1.4
	}
	return premium
}

// abyssalCurveCorrection fits the bounded per-family
// correction: the dynamics-weighted median ratio of observed
// asks to what the curve predicts for each comp's own score
// and source price, clamped to [0.7, 1.4]. Returns the
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
	if corr < 0.7 {
		corr = 0.7
	}
	if corr > 1.4 {
		corr = 1.4
	}
	return corr, len(ratios)
}

// estimateAbyssal is the pure estimator. Anchors in order:
// a corpus band (module scope when the item's own source
// module fills one, family scope otherwise), then the score
// curve over the source price with its bounded correction,
// then the source price alone as a labeled prior. No anchor,
// no estimate.
func estimateAbyssal(in abyssalEstimateInput) abyssalEstimate {
	est := abyssalEstimate{PremiumMult: 1}
	usable := make([]abyssalComp, 0, len(in.Comps))
	for _, c := range in.Comps {
		if c.Ask <= 0 {
			continue
		}
		if in.PrimaryAbs > 0 && c.PrimaryAbs > 0 {
			if r := c.PrimaryAbs / in.PrimaryAbs; r < 0.4 || r > 2.5 {
				continue
			}
		}
		usable = append(usable, c)
	}

	// Corpus: module scope first (listings of this module),
	// then the family's whole bucket. Scope is decided inside
	// the item's own score band — the only band the corpus
	// path ever reads — so a source with many listings at
	// other scores cannot claim module scope here.
	var sameSource []abyssalComp
	if in.SourceTypeID > 0 {
		band := abyssalBandOf(in.Score)
		for _, c := range usable {
			if c.SourceTypeID == in.SourceTypeID && abyssalBandOf(c.Score) == band {
				sameSource = append(sameSource, c)
			}
		}
	}
	scopeComps := usable
	scope := "family"
	if len(sameSource) >= abyssalCorpusMin {
		scopeComps = sameSource
		scope = "module"
	}
	if len(scopeComps) >= abyssalCorpusMin {
		stats, _ := abyssalBandStats(scopeComps)
		band := stats[abyssalBandOf(in.Score)]
		if band.median > 0 {
			premium := 1.0
			if scope == "family" {
				premium = abyssalProvenancePremium(in)
			}
			value := band.median * premium
			low, high := band.q1*premium, band.q3*premium
			if band.n < 3 {
				low, high = value*0.75, value*1.35
			}
			// A bucket drawn entirely from one other source
			// module carries that module's market with it.
			// When the bucket has no listing of the item's own
			// source and only one source at all, meet the
			// curve prior halfway (in log space) rather than
			// import another tier's price level whole.
			if scope == "family" && len(sameSource) == 0 && in.SourceTypical > 0 {
				sources := map[int64]bool{}
				for _, c := range scopeComps {
					sources[c.SourceTypeID] = true
				}
				if len(sources) == 1 {
					corr, _ := abyssalCurveCorrection(usable)
					prior := in.SourceTypical * abyssalCurveMult(in.Score) * corr
					if prior > 0 {
						value = math.Sqrt(value * prior)
						if prior < low {
							low = prior * 0.8
						}
						if prior*1.25 > high {
							high = prior * 1.25
						}
					}
				}
			}
			if low > value {
				low = value
			}
			if high < value {
				high = value
			}
			est.OK = true
			est.Value, est.Low, est.High = value, low, high
			est.Basis, est.Scope = "corpus", scope
			est.Comps = len(scopeComps)
			est.PremiumMult = premium
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

// abyssalBaseCandidate is one module a mutaplasmid can be
// applied to, with our price for it and its base attributes.
type abyssalBaseCandidate struct {
	SourceTypeID int64
	Typical      float64
	Base         map[int64]float64
}

// abyssalRecreationCost prices rolling another item at
// least this good: expected attempts times the cheapest
// reachable base plus the mutaplasmid. Reachable means the
// base's band can still produce the item's primary rolls.
// ok is false when no priced, reachable path exists.
func abyssalRecreationCost(sc abyssalScoring, in abyssalScoringInputs, target map[int64]float64, cands []abyssalBaseCandidate, mutatorTypical float64) (float64, float64, bool) {
	if !sc.HasScore || sc.Attempts <= 0 || mutatorTypical <= 0 {
		return 0, 0, false
	}
	var primaries []abyssalAttrResult
	for _, a := range sc.Attrs {
		if a.Scored && a.Primary {
			primaries = append(primaries, a)
		}
	}
	best := math.Inf(1)
	for _, cand := range cands {
		if cand.Typical <= 0 || cand.Base == nil {
			continue
		}
		reachable := true
		for _, p := range primaries {
			rng, ok := in.Ranges[p.AttributeID]
			if !ok {
				reachable = false
				break
			}
			base, ok := cand.Base[p.AttributeID]
			if !ok || base == 0 {
				reachable = false
				break
			}
			want, ok := target[p.AttributeID]
			if !ok {
				reachable = false
				break
			}
			high := true
			if hg, ok := in.HighGood[p.AttributeID]; ok {
				high = hg
			}
			if high && base*rng.MaxMult < want {
				reachable = false
				break
			}
			if !high && base*rng.MinMult > want {
				reachable = false
				break
			}
		}
		if !reachable {
			continue
		}
		if cost := sc.Attempts * (cand.Typical + mutatorTypical); cost < best {
			best = cost
		}
	}
	if math.IsInf(best, 1) {
		return 0, 0, false
	}
	return best, sc.Attempts, true
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

// abyssalSightingWeight: exit dynamics over ask levels. A
// listing still live counts once; one that vanished before
// its term counts more (leaving early is consistent with
// being accepted); one that expired counts least (it sat
// the whole term at that ask). None of it reads a departure
// as a sale.
func abyssalSightingWeight(departed bool, departKind string) float64 {
	if !departed {
		return 1.0
	}
	if departKind == "vanished" {
		return 1.25
	}
	return 0.6
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

	// Prices for everything the math can anchor on: due
	// items' sources and mutators, recreation candidate
	// bases, and corpus comps' sources (the correction's
	// denominators). One AppraiseLines call, house order.
	priceTypes := map[int64]bool{}
	for _, it := range due {
		priceTypes[it.SourceTypeID] = true
		priceTypes[it.MutatorTypeID] = true
		for _, src := range sourcesFor(it.MutatorTypeID) {
			priceTypes[src] = true
			baseFor(src)
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
		in := abyssalScoringInputs{
			Family:   family,
			Rolls:    rolls,
			Ranges:   rangesFor(it.MutatorTypeID),
			Base:     baseFor(it.SourceTypeID),
			HighGood: highGood,
			Weights:  weightsByFamily[family],
		}
		sc := scoreAbyssal(in)
		entries = append(entries, scoredItem{it: it, family: family, rolls: rolls, in: in, sc: sc})
		if sc.HasScore {
			batchScores[it.ItemID] = sc.Score
		}
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
			w := abyssalSightingWeight(r.DepartedAt.Valid, r.DepartKind.String)
			// A listing hugging our own stored estimate may
			// be anchored on it; halve its say.
			if r.EstValue.Valid && r.EstValue.Float64 > 0 {
				if d := math.Abs(r.Price-r.EstValue.Float64) / r.EstValue.Float64; d < 0.02 {
					w *= 0.5
				}
			}
			comps = append(comps, abyssalComp{
				ItemID:         r.ItemID,
				SourceTypeID:   r.SourceTypeID,
				Score:          score,
				Ask:            r.Price,
				SourceTypical:  priceByType[r.SourceTypeID],
				DynamicsWeight: w,
				PrimaryAbs:     r.PrimaryAbs.Float64,
			})
		}
		return comps
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
			est := estimateAbyssal(abyssalEstimateInput{
				Score:         sc.Score,
				SourceTypical: priceByType[it.SourceTypeID],
				SourceTypeID:  it.SourceTypeID,
				PrimaryAbs:    sc.PrimaryAbs,
				Comps:         compsFor(family, it.ItemID),
			})
			if est.OK {
				params.EstValue = sql.NullFloat64{Float64: est.Value, Valid: true}
				params.EstLow = sql.NullFloat64{Float64: est.Low, Valid: true}
				params.EstHigh = sql.NullFloat64{Float64: est.High, Valid: true}
				params.EstBasis = est.Basis
				params.EstScope = est.Scope
				params.EstComps = int64(est.Comps)
				params.PremiumMult = est.PremiumMult
			}
			// Recreation: the cheapest reachable base among
			// every source this mutaplasmid has been seen on.
			cands := make([]abyssalBaseCandidate, 0, 8)
			for _, src := range sourcesFor(it.MutatorTypeID) {
				cands = append(cands, abyssalBaseCandidate{
					SourceTypeID: src,
					Typical:      priceByType[src],
					Base:         baseFor(src),
				})
			}
			if cost, attempts, ok := abyssalRecreationCost(sc, in, rolls, cands, priceByType[it.MutatorTypeID]); ok {
				params.RecreationCost = sql.NullFloat64{Float64: cost, Valid: true}
				params.RecreationAttempts = sql.NullFloat64{Float64: attempts, Valid: true}
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
