package fit

import (
	"math"
	"sort"
)

// reactiveCycles bounds the adaptation run; the split repeats well
// inside it for every hull.
const reactiveCycles = 50

// adaptReactiveHardeners settles each running Reactive Armor
// Hardener on the split it reaches against even damage (a quarter
// of each type), the way the game shifts it, then folds again with
// that split. The ship's own armor resonances, without the
// hardener, decide which types it takes most of.
func (f *fitting) adaptReactiveHardeners() {
	var hardeners []*entity
	for _, ent := range f.fitted {
		if ent.kind == entModule && ent.cycling() && f.snap.HasEffect(ent.typeID, EffectAdaptiveArmorHardener) {
			hardeners = append(hardeners, ent)
		}
	}
	if len(hardeners) == 0 {
		return
	}
	isHardener := make(map[*entity]bool, len(hardeners))
	for _, e := range hardeners {
		isHardener[e] = true
	}

	all := f.apps
	without := make([]application, 0, len(all))
	for _, app := range all {
		if !(isHardener[app.source] && app.effectID == EffectAdaptiveArmorHardener) {
			without = append(without, app)
		}
	}
	f.apps = without
	f.resetCalc()
	f.foldAttributes()

	attrs := [4]int64{AttrArmorEM, AttrArmorThermal, AttrArmorKinetic, AttrArmorExplosive}
	var ship [4]float64
	for i, a := range attrs {
		ship[i] = f.shipAttr(a)
	}
	for _, e := range hardeners {
		var start [4]float64
		for i, a := range attrs {
			start[i] = e.get(f.snap, a)
		}
		adapted := adaptReactiveSplit(ship, start, e.get(f.snap, attrResistanceShift)/100)
		for i, a := range attrs {
			e.base[a] = adapted[i]
		}
	}

	f.apps = all
	f.resetCalc()
	f.foldAttributes()
}

// resetCalc clears every folded value so the fold can run again.
func (f *fitting) resetCalc() {
	for _, e := range f.entities {
		e.calc = map[int64]float64{}
	}
}

// adaptReactiveSplit runs the hardener's cycles: each cycle the two
// damage types the ship takes least of each give up to shift of
// resistance (all three when only one type is taken), and the rest
// share what was given. When the split repeats, the average over
// the repeating loop is what the hardener holds on average. ship and
// rah are resonances (1 - resistance), EM, thermal, kinetic,
// explosive.
func adaptReactiveSplit(ship, rah [4]float64, shift float64) [4]float64 {
	if shift <= 0 {
		return rah
	}
	history := make([][4]float64, 0, reactiveCycles)
	cur := rah
	for range reactiveCycles {
		for p, prev := range history {
			if sameSplit(prev, cur) {
				return averageSplits(history[p:])
			}
		}
		history = append(history, cur)

		var taken [4]float64
		for i := range taken {
			taken[i] = ship[i] * cur[i]
		}
		order := []int{0, 1, 2, 3}
		sort.SliceStable(order, func(a, b int) bool { return taken[order[a]] < taken[order[b]] })
		donors := 2
		if taken[order[2]] == 0 {
			donors = 3
		}
		given := 0.0
		for _, i := range order[:donors] {
			amt := math.Min(shift, 1-cur[i])
			cur[i] += amt
			given += amt
		}
		for _, i := range order[donors:] {
			cur[i] -= given / float64(4-donors)
		}
	}
	return averageSplits(history[len(history)/2:])
}

func sameSplit(a, b [4]float64) bool {
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func averageSplits(splits [][4]float64) [4]float64 {
	var out [4]float64
	for _, s := range splits {
		for i := range out {
			out[i] += s[i]
		}
	}
	for i := range out {
		out[i] /= float64(len(splits))
	}
	return out
}
