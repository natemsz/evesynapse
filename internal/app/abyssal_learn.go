package app

// ---------------------------------------------------------------------
// The learned correction (schema 070). The ladder prices an item by
// where its stats place it among ordinary modules; it cannot see
// what buyers pay extra for -- a "good enough" roll that clears
// above its rung position, a stat combination a fit needs. Where a
// family's clean listings and confirmed sales are dense enough to
// show it, a small boosted model learns that, as a correction to
// the ladder rather than a price from scratch:
//
//   value = ladder(index) x exp(shrink x g(stats)), within [0.6, 1.6]
//
// g is gradient-boosted regression trees on log(ask / ladder
// figure), fitted on each comp's own rolled stats (direction-signed
// log values, so "higher" is always "better"), with every split
// constrained monotone: a strictly better roll never earns a smaller
// correction, so the corrected price keeps the ladder's promise that
// a better module never prices lower. Its say shrinks toward none by
// the family's effective comp count, and the multiplier is clamped
// to the band either way.
//
// The model has to earn its place. Each pass fits it on a family's
// older comps and scores it on the newer ones it never saw, against
// the plain ladder-times-factor fitted on the same older comps; the
// correction is applied only where its median error beat that by a
// margin. Everywhere else the bounded per-family factor speaks, as
// before. The record of that test is stored per family and shown
// beside the estimate.
//
// An item's own ask never prices it: comps are split into folds by
// item, and an item in the corpus is corrected by the model fitted
// without its fold.
// ---------------------------------------------------------------------

import (
	"math"
	"sort"
	"time"

	db "evesynapse/internal/db/sqlc"
)

const (
	abyssalLearnMinComps   = 40   // clean comps a family needs before a fit is tried
	abyssalLearnMinHoldout = 10   // newer comps the backtest must score
	abyssalLearnMinNEff    = 25.0 // effective comps after weighting
	abyssalLearnPrior      = 40.0 // effective comps the correction needs to speak at half strength
	abyssalLearnBandMin    = 0.6  // the corrected figure's floor against the ladder
	abyssalLearnBandMax    = 1.6  // and its ceiling
	abyssalLearnMargin     = 0.97 // the model's error must be at most this share of the factor's
	abyssalLearnHoldout    = 0.25 // newest share of comps held out
	abyssalLearnFolds      = 5
	abyssalLearnTrimMADs   = 3.0 // comps further than this many MADs from the family's median are dropped
)

// abyssalLearnRow is one training comp: its features, its target
// log(ask / ladder figure), its say, and when it was first seen.
type abyssalLearnRow struct {
	ItemID int64
	X      []float64
	Y      float64
	W      float64
	Seen   time.Time
	Sold   bool
}

// abyssalLearnFeatures turns rolled stats into the model's
// features: one per family-weighted attribute, the log of its
// magnitude signed so higher is better. Missing is NaN.
func abyssalLearnFeatures(weights []db.AbyssalFamilyWeight, highGood map[int64]bool, rolls map[int64]float64) []float64 {
	x := make([]float64, len(weights))
	for i, w := range weights {
		v, ok := rolls[w.AttributeID]
		if !ok || v == 0 {
			x[i] = math.NaN()
			continue
		}
		f := math.Log(math.Abs(v))
		if v < 0 {
			f = -f // a larger negative magnitude is a lower value
		}
		if hg, ok := highGood[w.AttributeID]; ok && !hg {
			f = -f
		}
		x[i] = f
	}
	return x
}

// abyssalFamilyFitRecord is a family's backtest, as stored.
type abyssalFamilyFitRecord struct {
	Comps       int
	SalesN      int
	NEff        float64
	HoldoutN    int
	HasBacktest bool
	MAELadder   float64 // median absolute log error, ladder x factor
	MAEModel    float64 // median absolute log error, learned correction
	BiasModel   float64 // median signed log error (ask over prediction), learned correction
	UseModel    bool
}

// abyssalLearned is one family's fitted correction.
type abyssalLearned struct {
	Fit    abyssalFamilyFitRecord
	full   *abyssalGBM
	folds  []*abyssalGBM
	foldOf map[int64]int
	shrink float64
}

// Correction is the multiplier on the ladder figure for an item
// with features x: the model fitted without the item's own fold
// when the item is a comp. ok is false when the family's model is
// not in use.
func (l *abyssalLearned) Correction(itemID int64, x []float64) (float64, bool) {
	if l == nil || !l.Fit.UseModel || l.full == nil {
		return 1, false
	}
	m := l.full
	if k, ok := l.foldOf[itemID]; ok && l.folds[k] != nil {
		m = l.folds[k]
	}
	return abyssalLearnBand(l.shrink * m.Predict(x)), true
}

func abyssalLearnBand(logMult float64) float64 {
	return math.Min(math.Max(math.Exp(logMult), abyssalLearnBandMin), abyssalLearnBandMax)
}

// abyssalLearnFamily fits and tests one family's correction.
func abyssalLearnFamily(in []abyssalLearnRow) *abyssalLearned {
	rows := make([]abyssalLearnRow, 0, len(in))
	for _, r := range in {
		if r.W > 0 && !math.IsNaN(r.Y) && !math.IsInf(r.Y, 0) {
			rows = append(rows, r)
		}
	}
	rows = abyssalTrimOutliers(rows)
	l := &abyssalLearned{}
	l.Fit.Comps = len(rows)
	for _, r := range rows {
		if r.Sold {
			l.Fit.SalesN++
		}
	}
	l.Fit.NEff = abyssalLearnNEff(rows)
	if len(rows) < abyssalLearnMinComps {
		return l
	}

	// The backtest: older comps train, newer comps judge.
	sorted := append([]abyssalLearnRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Seen.Before(sorted[j].Seen) })
	hold := int(math.Ceil(float64(len(sorted)) * abyssalLearnHoldout))
	if hold < abyssalLearnMinHoldout {
		hold = abyssalLearnMinHoldout
	}
	train, test := sorted[:len(sorted)-hold], sorted[len(sorted)-hold:]
	logF, _ := abyssalShrunkLogFactor(abyssalLearnLogs(train))
	m := fitAbyssalGBM(train, abyssalGBMDefaults)
	shrinkTrain := abyssalLearnNEff(train) / (abyssalLearnNEff(train) + abyssalLearnPrior)
	var errLadder, errModel, signed []weightedValue
	for _, r := range test {
		pm := math.Log(abyssalLearnBand(shrinkTrain * m.Predict(r.X)))
		errLadder = append(errLadder, weightedValue{v: math.Abs(r.Y - logF), w: r.W})
		errModel = append(errModel, weightedValue{v: math.Abs(r.Y - pm), w: r.W})
		signed = append(signed, weightedValue{v: r.Y - pm, w: r.W})
	}
	l.Fit.HasBacktest = true
	l.Fit.HoldoutN = len(test)
	l.Fit.MAELadder = abyssalWeightedQuantile(errLadder, 0.5)
	l.Fit.MAEModel = abyssalWeightedQuantile(errModel, 0.5)
	l.Fit.BiasModel = abyssalWeightedQuantile(signed, 0.5)
	l.Fit.UseModel = l.Fit.NEff >= abyssalLearnMinNEff &&
		l.Fit.MAEModel < abyssalLearnMargin*l.Fit.MAELadder
	if !l.Fit.UseModel {
		return l
	}

	// In use: the full model for items outside the corpus, and
	// one model per fold for the comps themselves.
	l.full = fitAbyssalGBM(rows, abyssalGBMDefaults)
	l.shrink = l.Fit.NEff / (l.Fit.NEff + abyssalLearnPrior)
	l.foldOf = make(map[int64]int, len(rows))
	l.folds = make([]*abyssalGBM, abyssalLearnFolds)
	for _, r := range rows {
		l.foldOf[r.ItemID] = int(uint64(r.ItemID) % abyssalLearnFolds)
	}
	for k := 0; k < abyssalLearnFolds; k++ {
		var rest []abyssalLearnRow
		for _, r := range rows {
			if l.foldOf[r.ItemID] != k {
				rest = append(rest, r)
			}
		}
		if len(rest) > 0 {
			l.folds[k] = fitAbyssalGBM(rest, abyssalGBMDefaults)
		}
	}
	return l
}

// abyssalTrimOutliers drops comps whose target sits more than
// abyssalLearnTrimMADs robust deviations from the family's
// weighted median: joke asks and mispastes, before any fit sees
// them.
func abyssalTrimOutliers(rows []abyssalLearnRow) []abyssalLearnRow {
	if len(rows) < 5 {
		return rows
	}
	med := abyssalWeightedQuantile(abyssalLearnLogs(rows), 0.5)
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

func abyssalLearnLogs(rows []abyssalLearnRow) []weightedValue {
	out := make([]weightedValue, len(rows))
	for i, r := range rows {
		out[i] = weightedValue{v: r.Y, w: r.W}
	}
	return out
}

func abyssalLearnNEff(rows []abyssalLearnRow) float64 {
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

// ---------------------------------------------------------------------
// Monotone gradient boosting: squared loss on weighted rows, shallow
// trees, every feature constrained non-decreasing. A split is kept
// only when its left leaf's value does not exceed its right's, and
// each child subtree is bounded at the midpoint between them (the
// XGBoost construction), so every tree -- and their sum -- is
// non-decreasing in every feature.
// ---------------------------------------------------------------------

type abyssalGBMParams struct {
	Rounds   int
	Depth    int
	LR       float64
	Lambda   float64 // L2 on leaf values, in normalized-weight units
	MinLeafW float64 // least normalized weight a leaf may hold
}

var abyssalGBMDefaults = abyssalGBMParams{Rounds: 80, Depth: 3, LR: 0.1, Lambda: 2, MinLeafW: 5}

type abyssalGBMNode struct {
	Leaf        bool
	Val         float64
	Feat        int
	Thr         float64
	Left, Right int
}

type abyssalGBMTree []abyssalGBMNode

func (t abyssalGBMTree) eval(x []float64) float64 {
	i := 0
	for !t[i].Leaf {
		if x[t[i].Feat] <= t[i].Thr {
			i = t[i].Left
		} else {
			i = t[i].Right
		}
	}
	return t[i].Val
}

type abyssalGBM struct {
	Base  float64
	LR    float64
	Fill  []float64 // per-feature fill for missing values
	Trees []abyssalGBMTree
}

// Predict is the model's log correction for x.
func (m *abyssalGBM) Predict(x []float64) float64 {
	if m == nil {
		return 0
	}
	xf := m.filled(x)
	p := m.Base
	for _, t := range m.Trees {
		p += m.LR * t.eval(xf)
	}
	return p
}

func (m *abyssalGBM) filled(x []float64) []float64 {
	xf := make([]float64, len(m.Fill))
	for j := range xf {
		if j < len(x) && !math.IsNaN(x[j]) {
			xf[j] = x[j]
		} else {
			xf[j] = m.Fill[j]
		}
	}
	return xf
}

func fitAbyssalGBM(rows []abyssalLearnRow, p abyssalGBMParams) *abyssalGBM {
	if len(rows) == 0 {
		return nil
	}
	nf := len(rows[0].X)
	m := &abyssalGBM{LR: p.LR, Fill: make([]float64, nf)}
	for j := 0; j < nf; j++ {
		var col []float64
		for _, r := range rows {
			if j < len(r.X) && !math.IsNaN(r.X[j]) {
				col = append(col, r.X[j])
			}
		}
		if len(col) > 0 {
			sort.Float64s(col)
			m.Fill[j] = col[len(col)/2]
		}
	}
	X := make([][]float64, len(rows))
	w := make([]float64, len(rows))
	sw := 0.0
	for i, r := range rows {
		X[i] = m.filled(r.X)
		sw += r.W
	}
	for i, r := range rows {
		w[i] = r.W * float64(len(rows)) / sw // mean weight 1
	}
	m.Base = abyssalWeightedQuantile(abyssalLearnLogs(rows), 0.5)
	pred := make([]float64, len(rows))
	for i := range pred {
		pred[i] = m.Base
	}
	res := make([]float64, len(rows))
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	for round := 0; round < p.Rounds; round++ {
		for i, r := range rows {
			res[i] = r.Y - pred[i]
		}
		var t abyssalGBMTree
		abyssalGrow(&t, X, res, w, idx, p.Depth, math.Inf(-1), math.Inf(1), p)
		if len(t) == 1 && t[0].Val == 0 {
			break // nothing left to learn
		}
		m.Trees = append(m.Trees, t)
		for i := range pred {
			pred[i] += p.LR * t.eval(X[i])
		}
	}
	return m
}

// abyssalGrow grows one node over rows idx and returns its index.
// lo and hi bound the node's values (the monotone envelope).
func abyssalGrow(t *abyssalGBMTree, X [][]float64, r, w []float64, idx []int, depth int, lo, hi float64, p abyssalGBMParams) int {
	G, H := 0.0, 0.0
	for _, i := range idx {
		G += w[i] * r[i]
		H += w[i]
	}
	clampV := func(v float64) float64 { return math.Min(math.Max(v, lo), hi) }
	id := len(*t)
	*t = append(*t, abyssalGBMNode{Leaf: true, Val: clampV(G / (H + p.Lambda))})
	if depth == 0 || H < 2*p.MinLeafW || len(X) == 0 {
		return id
	}
	parent := G * G / (H + p.Lambda)
	bestGain, bestFeat, bestThr := 1e-9, -1, 0.0
	var bestL, bestR float64
	order := append([]int(nil), idx...)
	for j := range X[0] {
		sort.Slice(order, func(a, b int) bool { return X[order[a]][j] < X[order[b]][j] })
		gl, hl := 0.0, 0.0
		for k := 0; k < len(order)-1; k++ {
			i := order[k]
			gl += w[i] * r[i]
			hl += w[i]
			if X[order[k+1]][j] == X[i][j] {
				continue // no threshold between equal values
			}
			gr, hr := G-gl, H-hl
			if hl < p.MinLeafW || hr < p.MinLeafW {
				continue
			}
			vl, vr := clampV(gl/(hl+p.Lambda)), clampV(gr/(hr+p.Lambda))
			if vl > vr {
				continue // a better roll may never earn less
			}
			gain := gl*gl/(hl+p.Lambda) + gr*gr/(hr+p.Lambda) - parent
			if gain > bestGain {
				bestGain, bestFeat = gain, j
				bestThr = (X[i][j] + X[order[k+1]][j]) / 2
				bestL, bestR = vl, vr
			}
		}
	}
	if bestFeat < 0 {
		return id
	}
	var li, ri []int
	for _, i := range idx {
		if X[i][bestFeat] <= bestThr {
			li = append(li, i)
		} else {
			ri = append(ri, i)
		}
	}
	mid := (bestL + bestR) / 2
	left := abyssalGrow(t, X, r, w, li, depth-1, lo, mid, p)
	right := abyssalGrow(t, X, r, w, ri, depth-1, mid, hi, p)
	(*t)[id] = abyssalGBMNode{Feat: bestFeat, Thr: bestThr, Left: left, Right: right}
	return id
}
