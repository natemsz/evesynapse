package app

import "math"

// The weighted statistics the abyssal and true value learners share:
// effective sample counts, the robust outlier trim, and the shrunk,
// clamped log factor. Both learners train on rows whose target is a
// weighted log figure.

// learnRow is a training row with a weighted target.
type learnRow interface {
	target() weightedValue
}

func (r abyssalLearnRow) target() weightedValue { return weightedValue{v: r.Y, w: r.W} }
func (r tvLearnRow) target() weightedValue      { return weightedValue{v: r.Y, w: r.W} }

// learnTargets returns the rows' weighted targets.
func learnTargets[R learnRow](rows []R) []weightedValue {
	out := make([]weightedValue, len(rows))
	for i, r := range rows {
		out[i] = r.target()
	}
	return out
}

// learnNEff is the rows' effective sample count, (sum w)^2 / sum w^2:
// equal to the row count when every weight is the same, smaller when
// a few rows carry most of the weight. It is 0 for no weight.
func learnNEff[R learnRow](rows []R) float64 {
	sw, sw2 := 0.0, 0.0
	for _, r := range rows {
		w := r.target().w
		sw += w
		sw2 += w * w
	}
	if sw2 <= 0 {
		return 0
	}
	return sw * sw / sw2
}

// trimLearnOutliers drops rows whose target sits more than
// abyssalLearnTrimMADs robust deviations (1.4826 times the weighted
// median absolute deviation, floored at 0.05) from the set's weighted
// median: joke asks and mispastes, before any fit sees them. Sets of
// fewer than five rows are returned whole.
func trimLearnOutliers[R learnRow](rows []R) []R {
	if len(rows) < 5 {
		return rows
	}
	med := abyssalWeightedQuantile(learnTargets(rows), 0.5)
	dev := make([]weightedValue, len(rows))
	for i, r := range rows {
		t := r.target()
		dev[i] = weightedValue{v: math.Abs(t.v - med), w: t.w}
	}
	mad := math.Max(1.4826*abyssalWeightedQuantile(dev, 0.5), 0.05)
	out := rows[:0:0]
	for _, r := range rows {
		if math.Abs(r.target().v-med) <= abyssalLearnTrimMADs*mad {
			out = append(out, r)
		}
	}
	return out
}

// shrunkLogFactor is a bounded correction over weighted log ratios:
// the weighted median, shrunk toward no correction by
// nEff / (nEff + prior), clamped to [log(lo), log(hi)]. It also
// returns the effective count; with no weight both are 0.
func shrunkLogFactor(logs []weightedValue, prior, lo, hi float64) (logFactor, nEff float64) {
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
	shrunk := med * nEff / (nEff + prior)
	return math.Min(math.Max(shrunk, math.Log(lo)), math.Log(hi)), nEff
}
