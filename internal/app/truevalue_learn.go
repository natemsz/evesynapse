package app

// ---------------------------------------------------------------------
// The learned correction for contract-only ships: the abyssal
// method (abyssal_learn.go) adapted to hulls. Abyssals price a
// module as a deterministic ladder figure times a correction a
// monotone boosted model learns from the family's comps; ships
// price the same way. The ladder is the hull's build cost (the
// true-value BOM figure), the family is the hull's class -- its
// SDE group, the grouping the stored SDE carries -- and the
// comps are the contract samples the evidence tiers collect
// (truevalue_worker.go): confirmed sales heaviest, then likely
// sales, then live asks, exactly the abyssal corpus's weights
// (abyssalSightingWeight, abyssalAgeWeight). Age is a weight
// here, never a cutoff: hulls move slowly, and the tiered
// path's hard 14-day sample window is what kept real ship
// estimates lapsing -- a confirmed sale from weeks ago still
// counts in these fits, on the slowest of the per-tier age
// curves (tvSampleWeight), because it remains the strongest
// evidence a hull has.
//
//   value = build cost x correction, correction within [0.5, 6]
//
// The model is gradient-boosted regression trees on
// log(price / build cost) -- the repo's own booster, shared
// with the abyssal fit -- over one feature, log(build cost):
// how a hull's premium over its build cost moves with the
// hull's scale, constrained non-decreasing like every abyssal
// split. It has to earn its place the same way: fitted on a
// family's older comps, scored on the newer ones against the
// family's bounded shrunk factor fitted on the same older
// comps, and applied only where its median error beat that
// factor by the margin. Where it does not, the factor speaks:
// at family grain when the family's comps support one, else
// the hull's own, from its own samples alone.
//
// The modification hulls need and modules never did: a ladder
// that is fiction. A prize hull's blueprint prices its nominal
// materials (a Cambion "builds" for 548,521 ISK), and some
// hulls have no blueprint at all; no correction band reaches
// their real prices from there, and multiplying fiction by a
// learned factor would still be fiction. When a hull's samples
// unanimously refute its ladder -- none inside the sampler's
// band -- or it has none, the hull leaves the ladder behind
// and is priced by a learned LEVEL: the weighted mean of its
// samples' log prices, shrunk toward the family's other
// level-priced hulls. The level is part of the same fit, not
// a separate estimator: one stored estimate per type, from
// one model, whichever mode priced it.
//
// A single sample never prices anything, in any mode: a hull
// with no trainable evidence stores no estimate at all.
// ---------------------------------------------------------------------

import (
	"math"
	"sort"
	"time"
)

const (
	tvLearnMinComps   = 10   // family rows before a fit is tried
	tvLearnMinHoldout = 4    // newer rows the backtest must score
	tvLearnMinNEff    = 6.0  // effective rows the model needs behind it
	tvLearnPrior      = 6.0  // effective rows the model's say needs to reach half strength (the abyssal prior is 40, for families with hundreds of comps; a ship family pools a handful of hulls, and 6 reaches the same effective shrink at those counts)
	tvLearnBandMin    = 0.5  // the corrected figure's floor against the ladder
	tvLearnBandMax    = 6.0  // and its ceiling: capital premiums over build cost run wider than module ones
	tvLearnMargin     = 0.97 // the model's error must be at most this share of the factor's
	tvLearnHoldout    = 0.25 // newest share of rows held out

	tvFactorPrior   = 6.0 // effective rows a bounded factor needs to move halfway
	tvFactorMinRows = 4   // family rows before the family factor may speak
	tvFactorMinNEff = 3.0

	tvOwnMinRows = 2   // a hull's own rows before its own factor may speak
	tvOwnMinNEff = 1.5 // their effective count

	tvLevelPrior      = 2.0 // weight of the family peer level inside a hull's level
	tvLevelSoloNEff   = 3.0 // effective samples a level with no peers to shrink toward needs
	tvLevelMinSamples = 2   // a single sample never prices anything
)

// tvVanishedWeight is a likely sale's say in a fit: the
// abyssal corpus's weight for a listing that vanished before
// its term without a confirmed sale (abyssalSightingWeight).
const tvVanishedWeight = 1.25

// The age curves, per tier: the abyssal decay's shape
// (abyssalAgeWeight -- an exponential halving floored above
// zero) with a longer memory, because hulls move slowly and
// their evidence is scarce. A confirmed sale's say halves
// every 90 days and never falls below 0.40 of fresh: even
// fully aged, a sale (2.5 x 0.40 = 1.0) speaks as loudly as
// a fresh ask. A likely sale halves every 60 days to a 0.25
// floor. A live ask keeps the abyssal 30-day half-life --
// asks go stale fastest -- but floors at 0.20, twice the
// abyssal floor.
const (
	tvSaleHalfLife     = 90 * 24 * time.Hour
	tvSaleFloor        = 0.40
	tvVanishedHalfLife = 60 * 24 * time.Hour
	tvVanishedFloor    = 0.25
	tvAskHalfLife      = 30 * 24 * time.Hour
	tvAskFloor         = 0.20
)

// tvSampleWeight is a contract sample's say in a learned
// fit: its tier's weight (a confirmed sale counts most,
// abyssalSoldWeight; a vanished listing a little more than
// a live ask) decayed by its tier's age curve. Age is
// always this weight, never a cutoff.
func tvSampleWeight(s tvContractSample, now time.Time) float64 {
	w, halfLife, floor := 1.0, tvAskHalfLife, tvAskFloor
	switch s.source {
	case tvSampleFulfilled:
		w, halfLife, floor = abyssalSoldWeight, tvSaleHalfLife, tvSaleFloor
	case tvSampleVanished:
		w, halfLife, floor = tvVanishedWeight, tvVanishedHalfLife, tvVanishedFloor
	}
	age := now.Sub(s.at)
	if age < 0 {
		age = 0
	}
	decay := math.Pow(0.5, float64(age)/float64(halfLife))
	if decay < floor {
		decay = floor
	}
	return w * decay
}

// tvLearnRow is one training comp for a family's fit: a
// hull's contract sample expressed against its ladder.
type tvLearnRow struct {
	TypeID int64
	X      []float64 // [log ladder]
	Y      float64   // log(price / ladder); log(price) for level rows
	W      float64
	Seen   time.Time
	Source string
	Sold   bool
}

func tvRowsToAbyssal(rows []tvLearnRow) []abyssalLearnRow {
	out := make([]abyssalLearnRow, len(rows))
	for i, r := range rows {
		out[i] = abyssalLearnRow{ItemID: r.TypeID, X: r.X, Y: r.Y, W: r.W, Seen: r.Seen, Sold: r.Sold}
	}
	return out
}

func tvLearnLogs(rows []tvLearnRow) []weightedValue {
	out := make([]weightedValue, len(rows))
	for i, r := range rows {
		out[i] = weightedValue{v: r.Y, w: r.W}
	}
	return out
}

func tvLearnNEff(rows []tvLearnRow) float64 {
	sw, sw2 := 0.0, 0.0
	for _, r := range rows {
		sw += r.W
		sw2 += r.W * r.W
	}
	if sw2 <= 0 {
		return 0
	}
	return sw * sw / sw2
}

// tvTrimRows drops rows whose target sits more than
// abyssalLearnTrimMADs robust deviations from the set's
// weighted median -- the abyssal corpus's outlier rule
// (abyssalTrimOutliers), applied to ship rows: joke asks and
// mispastes, before any fit sees them.
func tvTrimRows(rows []tvLearnRow) []tvLearnRow {
	if len(rows) < 5 {
		return rows
	}
	med := abyssalWeightedQuantile(tvLearnLogs(rows), 0.5)
	dev := make([]weightedValue, len(rows))
	for i, r := range rows {
		dev[i] = weightedValue{v: math.Abs(r.Y - med), w: r.W}
	}
	mad := math.Max(1.4826*abyssalWeightedQuantile(dev, 0.5), 0.05)
	out := rows[:0:0]
	for _, r := range rows {
		if math.Abs(r.Y-med) <= abyssalLearnTrimMADs*mad {
			out = append(out, r)
		}
	}
	return out
}

// tvShrunkLogFactor is the bounded factor's arithmetic over
// weighted log ratios: the weighted median, shrunk toward no
// correction by the effective count, clamped to the band.
// The ship-width counterpart of abyssalShrunkLogFactor,
// shared by the family factor, the hull's own factor, and
// the backtest that judges the model against exactly this.
func tvShrunkLogFactor(rows []tvLearnRow, prior float64) (logFactor, nEff float64) {
	nEff = tvLearnNEff(rows)
	if len(rows) == 0 || nEff <= 0 {
		return 0, 0
	}
	med := abyssalWeightedQuantile(tvLearnLogs(rows), 0.5)
	shrunk := med * nEff / (nEff + prior)
	return math.Min(math.Max(shrunk, math.Log(tvLearnBandMin)), math.Log(tvLearnBandMax)), nEff
}

func tvLearnBand(logMult float64) float64 {
	return math.Min(math.Max(math.Exp(logMult), tvLearnBandMin), tvLearnBandMax)
}

// tvTierPlurality names the evidence tier an estimate rests
// on: the tier carrying the greatest total weight among the
// rows that priced it (ties go to the stronger tier), and
// how many samples that tier contributed. It feeds the
// stored row's provenance (trueValueBasis).
func tvTierPlurality(rows []tvLearnRow) (source string, count int) {
	weight := make(map[string]float64)
	n := make(map[string]int)
	for _, r := range rows {
		weight[r.Source] += r.W
		n[r.Source]++
	}
	for _, tier := range []string{tvSampleFulfilled, tvSampleVanished, tvSampleListing} {
		if w, ok := weight[tier]; ok && (source == "" || w > weight[source]) {
			source, count = tier, n[tier]
		}
	}
	return source, count
}

// tvShipFamilyFit is one family's fitted correction, with
// its backtest record (the pass logs it, as the abyssal pass
// stores its own per family).
type tvShipFamilyFit struct {
	rows  []tvLearnRow // trimmed training rows
	comps int
	nEff  float64

	useModel  bool
	model     *abyssalGBM
	shrink    float64
	logFactor float64
	factorOK  bool

	hasBacktest bool
	holdoutN    int
	maeFactor   float64
	maeModel    float64
}

// tvLearnFamily fits and tests one family's correction,
// mirroring abyssalLearnFamily: the backtest trains on the
// older rows and judges on the newer ones, the model is used
// only where it beat the factor by the margin, and the full
// fit then serves every hull in the family.
func tvLearnFamily(in []tvLearnRow) *tvShipFamilyFit {
	rows := make([]tvLearnRow, 0, len(in))
	for _, r := range in {
		if r.W > 0 && !math.IsNaN(r.Y) && !math.IsInf(r.Y, 0) {
			rows = append(rows, r)
		}
	}
	rows = tvTrimRows(rows)
	f := &tvShipFamilyFit{rows: rows, comps: len(rows)}
	f.nEff = tvLearnNEff(rows)
	if len(rows) >= tvFactorMinRows && f.nEff >= tvFactorMinNEff {
		f.logFactor, _ = tvShrunkLogFactor(rows, tvFactorPrior)
		f.factorOK = true
	}
	if len(rows) < tvLearnMinComps {
		return f
	}

	sorted := append([]tvLearnRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seen.Before(sorted[j].Seen) })
	hold := int(math.Ceil(float64(len(sorted)) * tvLearnHoldout))
	if hold < tvLearnMinHoldout {
		hold = tvLearnMinHoldout
	}
	if len(sorted)-hold < 2 {
		return f
	}
	train, test := sorted[:len(sorted)-hold], sorted[len(sorted)-hold:]
	logF, _ := tvShrunkLogFactor(train, tvFactorPrior)
	m := fitAbyssalGBM(tvRowsToAbyssal(train), abyssalGBMDefaults)
	shrinkTrain := tvLearnNEff(train) / (tvLearnNEff(train) + tvLearnPrior)
	var errFactor, errModel []weightedValue
	for _, r := range test {
		pm := math.Log(tvLearnBand(shrinkTrain * m.Predict(r.X)))
		errFactor = append(errFactor, weightedValue{v: math.Abs(r.Y - logF), w: r.W})
		errModel = append(errModel, weightedValue{v: math.Abs(r.Y - pm), w: r.W})
	}
	f.hasBacktest = true
	f.holdoutN = len(test)
	f.maeFactor = abyssalWeightedQuantile(errFactor, 0.5)
	f.maeModel = abyssalWeightedQuantile(errModel, 0.5)
	if f.nEff < tvLearnMinNEff || f.maeModel >= tvLearnMargin*f.maeFactor {
		return f
	}
	f.useModel = true
	f.model = fitAbyssalGBM(tvRowsToAbyssal(rows), abyssalGBMDefaults)
	f.shrink = f.nEff / (f.nEff + tvLearnPrior)
	return f
}

// tvShipHullFit is one hull's place in its family's fit:
// ladder-priced, or level-priced when its ladder is fiction
// or absent, with the rows and level the pricing step reads.
type tvShipHullFit struct {
	family  string
	ladder  float64
	level   bool
	samples int // every fresh sample the hull has, any tier

	ownRows []tvLearnRow // ladder mode: the hull's in-band rows

	levelOK  bool
	levelEst float64
	levelSrc string
	levelN   int
}

// tvShipPricing is one pass's fitted model for every thin
// ship type: families fitted once, hulls priced from them.
type tvShipPricing struct {
	families map[string]*tvShipFamilyFit
	hulls    map[int64]*tvShipHullFit
}

// price resolves one hull's estimated sell from the fit: the
// learned correction where the model earned its place, else
// the family factor, else the hull's own factor, else -- for
// a level hull -- its learned level. ok is false when no
// trainable evidence supports a figure.
func (p *tvShipPricing) price(typeID int64) (est float64, source string, count int, ok bool) {
	h := p.hulls[typeID]
	if h == nil {
		return 0, "", 0, false
	}
	if h.level {
		if h.levelOK {
			return h.levelEst, h.levelSrc, h.levelN, true
		}
		return 0, "", h.samples, false
	}
	fam := p.families[h.family]
	if fam != nil && fam.useModel && fam.model != nil {
		mult := tvLearnBand(fam.shrink * fam.model.Predict([]float64{math.Log(h.ladder)}))
		source, count = tvTierPlurality(fam.rows)
		return h.ladder * mult, source, count, true
	}
	if fam != nil && fam.factorOK {
		source, count = tvTierPlurality(fam.rows)
		return h.ladder * math.Exp(fam.logFactor), source, count, true
	}
	if len(h.ownRows) >= tvOwnMinRows {
		if lf, nEff := tvShrunkLogFactor(h.ownRows, tvFactorPrior); nEff >= tvOwnMinNEff {
			source, count = tvTierPlurality(h.ownRows)
			return h.ladder * math.Exp(lf), source, count, true
		}
	}
	return 0, "", h.samples, false
}

// buildTVShipPricing classifies every subject hull (ladder
// or level), pools the ladder hulls' rows into family fits,
// and settles each level hull's level against its family's
// other level hulls.
func buildTVShipPricing(subjects map[int64]bool, ladders map[int64]float64, samplesByType map[int64][]tvContractSample, families map[int64]string, now time.Time) *tvShipPricing {
	p := &tvShipPricing{
		families: make(map[string]*tvShipFamilyFit),
		hulls:    make(map[int64]*tvShipHullFit, len(subjects)),
	}
	rowOf := func(typeID int64, ladder float64, s tvContractSample) tvLearnRow {
		return tvLearnRow{
			TypeID: typeID, X: []float64{math.Log(ladder)}, Y: math.Log(s.price / ladder),
			W: tvSampleWeight(s, now), Seen: s.at, Source: s.source, Sold: s.source == tvSampleFulfilled,
		}
	}
	familyRows := make(map[string][]tvLearnRow)
	for typeID := range subjects {
		ladder := ladders[typeID]
		samples := samplesByType[typeID]
		h := &tvShipHullFit{family: families[typeID], ladder: ladder, samples: len(samples)}
		var inBand []tvContractSample
		if ladder > 0 {
			for _, s := range samples {
				if s.price >= tvContractBandLow*ladder && s.price <= tvContractBandHigh*ladder {
					inBand = append(inBand, s)
				}
			}
		}
		if ladder <= 0 || (len(samples) > 0 && len(inBand) == 0) {
			// No ladder, or a ladder every sample refutes:
			// the hull is priced by its level, not corrected
			// from a fiction anchor.
			h.level = true
		} else {
			for _, s := range inBand {
				h.ownRows = append(h.ownRows, rowOf(typeID, ladder, s))
			}
			familyRows[h.family] = append(familyRows[h.family], h.ownRows...)
		}
		p.hulls[typeID] = h
	}
	for family, rows := range familyRows {
		p.families[family] = tvLearnFamily(rows)
	}

	// Levels: each level hull's surviving samples (outliers
	// trimmed in log-price space) against the pooled levels
	// of its family's other level hulls.
	levelRows := make(map[int64][]tvLearnRow)
	familyLevelPool := make(map[string][]tvLearnRow)
	for typeID, h := range p.hulls {
		if !h.level {
			continue
		}
		var rows []tvLearnRow
		for _, s := range samplesByType[typeID] {
			if s.price > 0 {
				rows = append(rows, tvLearnRow{
					TypeID: typeID, Y: math.Log(s.price),
					W: tvSampleWeight(s, now), Seen: s.at, Source: s.source, Sold: s.source == tvSampleFulfilled,
				})
			}
		}
		rows = tvTrimRows(rows)
		levelRows[typeID] = rows
		familyLevelPool[h.family] = append(familyLevelPool[h.family], rows...)
	}
	for typeID, h := range p.hulls {
		if !h.level {
			continue
		}
		rows := levelRows[typeID]
		if len(rows) < tvLevelMinSamples {
			continue
		}
		var ownW, ownSum float64
		for _, r := range rows {
			ownW += r.W
			ownSum += r.W * r.Y
		}
		if ownW <= 0 {
			continue
		}
		var peerW, peerSum float64
		for _, r := range familyLevelPool[h.family] {
			if r.TypeID != typeID {
				peerW += r.W
				peerSum += r.W * r.Y
			}
		}
		var level float64
		switch {
		case peerW > 0:
			level = (ownSum + tvLevelPrior*(peerSum/peerW)) / (ownW + tvLevelPrior)
		case tvLearnNEff(rows) >= tvLevelSoloNEff:
			level = ownSum / ownW
		default:
			continue // no peers to shrink toward, too little of its own
		}
		h.levelEst = math.Exp(level)
		h.levelSrc, h.levelN = tvTierPlurality(rows)
		h.levelOK = true
	}
	return p
}
