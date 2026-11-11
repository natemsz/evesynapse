package fitview

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"evesynapse/internal/fit"
)

// fitRangeTarget is one of the damage-at-range chart's targets. A
// preset is a typical hull of its class, moving straight across the
// line of fire at its usual speed without a propulsion module.
type fitRangeTarget struct {
	Key    string      `json:"key"`
	Label  string      `json:"label"`
	Note   string      `json:"note"`
	target *fit.Target // nil: a target every weapon fully hits
}

var fitRangeTargets = []fitRangeTarget{
	{Key: "any", Label: "Any target", Note: "Reach only: every hit lands in full"},
	{Key: "frigate", Label: "Frigate", Note: "35 m signature, crossing at 400 m/s", target: &fit.Target{Signature: 35, Velocity: 400}},
	{Key: "cruiser", Label: "Cruiser", Note: "130 m signature, crossing at 250 m/s", target: &fit.Target{Signature: 130, Velocity: 250}},
	{Key: "battleship", Label: "Battleship", Note: "400 m signature, crossing at 120 m/s", target: &fit.Target{Signature: 400, Velocity: 120}},
}

// fitRangeSeries is the most weapon groups the chart draws on their
// own; the rest fold into one "Other weapons" line.
const fitRangeSeries = 4

// fitRangeSamples is how many even steps the distance axis takes;
// each weapon's hard reach adds its own edge points on top.
const fitRangeSamples = 60

// FitRangeData is the damage-at-range chart's data: the distance
// axis, the targets, the weapon groups, and for every target the
// total and each group's damage per second at each distance.
type FitRangeData struct {
	EndKm   float64                `json:"end"`
	Km      []float64              `json:"km"`
	Targets []fitRangeTarget       `json:"targets"`
	Groups  []FitRangeGroup        `json:"groups"`
	Curves  map[string][][]float64 `json:"curves"` // target key -> [total, group...]
}

// FitRangeGroup is one weapon group line: what it is and how far it
// reaches.
type FitRangeGroup struct {
	Label string `json:"label"`
	Reach string `json:"reach"`
}

type fitRangeGroup struct {
	FitRangeGroup
	weapons []fit.WeaponReach
}

// FitRange builds the chart's data from a simulated fit, the JSON the
// workbench's chart reads, and the inline sparkline's polyline points
// (any target, in a 120 x 28 box). All three are empty for a fit that
// does no damage.
func FitRange(res *fit.Result, nameOf func(int64) string) (data *FitRangeData, js, spark string) {
	if res == nil || res.DPS <= 0 || len(res.Weapons) == 0 {
		return nil, "", ""
	}
	groups := fitRangeGroups(res.Weapons, nameOf)
	end := 0.0
	unbounded := false
	for _, w := range res.Weapons {
		e := w.ReachEnd()
		if e <= 0 {
			unbounded = true
		}
		end = math.Max(end, e)
	}
	if unbounded {
		end = math.Max(end, 60000)
	}
	// The axis ends on the gridline past the longest reach.
	reachKm := math.Max(end*1.02, 5000) / 1000
	step := fitNiceStep(reachKm / 4)
	endKm := math.Min(math.Ceil(reachKm/step)*step, 300)

	// Even steps plus both sides of every hard edge, so a missile's
	// cut-off draws as a cliff rather than a slope.
	xs := make([]float64, 0, fitRangeSamples+1+2*len(res.Weapons))
	for i := 0; i <= fitRangeSamples; i++ {
		xs = append(xs, endKm*float64(i)/fitRangeSamples)
	}
	for _, w := range res.Weapons {
		var edge float64
		switch w.Kind {
		case fit.ReachMissile, fit.ReachSmartbomb, fit.ReachDrone, fit.ReachSentry:
			edge = w.Range
		case fit.ReachArea:
			edge = w.Optimal
		case fit.ReachTurret:
			if w.Falloff <= 0 {
				edge = w.Optimal
			}
		}
		if edge > 0 && edge/1000 < endKm {
			xs = append(xs, edge/1000, edge/1000+endKm/2000)
		}
	}
	sort.Float64s(xs)
	xs = dedupeKm(xs)

	data = &FitRangeData{EndKm: endKm, Km: xs, Targets: fitRangeTargets, Curves: map[string][][]float64{}}
	for _, g := range groups {
		data.Groups = append(data.Groups, g.FitRangeGroup)
	}
	for _, tg := range fitRangeTargets {
		rows := make([][]float64, len(groups)+1)
		for i := range rows {
			rows[i] = make([]float64, len(xs))
		}
		for xi, km := range xs {
			total := 0.0
			for gi, g := range groups {
				v := 0.0
				for _, w := range g.weapons {
					v += w.DamageAt(km*1000, tg.target)
				}
				rows[gi+1][xi] = round1(v)
				total += v
			}
			rows[0][xi] = round1(total)
		}
		data.Curves[tg.Key] = rows
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, "", ""
	}
	return data, string(b), fitRangeSpark(xs, data.Curves["any"][0], endKm)
}

// fitRangeGroups merges the engine's weapon entries by type and kind
// (the same gun in two states is one line), labels them, and folds
// past fitRangeSeries into one line.
func fitRangeGroups(weapons []fit.WeaponReach, nameOf func(int64) string) []fitRangeGroup {
	var groups []fitRangeGroup
	index := map[string]int{}
	counts := map[string]int{}
	for _, w := range weapons {
		key := fmt.Sprintf("%d/%s", w.TypeID, w.Kind)
		if i, ok := index[key]; ok {
			groups[i].weapons = append(groups[i].weapons, w)
			counts[key] += w.Count
			continue
		}
		index[key] = len(groups)
		counts[key] = w.Count
		groups = append(groups, fitRangeGroup{FitRangeGroup: FitRangeGroup{Reach: fitReachNote(w)}, weapons: []fit.WeaponReach{w}})
	}
	for key, i := range index {
		name := nameOf(groups[i].weapons[0].TypeID)
		if n := counts[key]; n > 1 {
			name += fmt.Sprintf(" ×%d", n)
		}
		groups[i].Label = name
	}
	if len(groups) > fitRangeSeries {
		other := fitRangeGroup{FitRangeGroup: FitRangeGroup{Label: "Other weapons", Reach: "Their lines summed"}}
		for _, g := range groups[fitRangeSeries-1:] {
			other.weapons = append(other.weapons, g.weapons...)
		}
		groups = append(groups[:fitRangeSeries-1], other)
	}
	return groups
}

// fitReachNote says in a few words how far a weapon group reaches.
func fitReachNote(w fit.WeaponReach) string {
	km := func(m float64) string { return FitFmt1(m/1000) + " km" }
	switch w.Kind {
	case fit.ReachTurret:
		if w.Falloff <= 0 {
			return "Optimal " + km(w.Optimal) + ", no falloff"
		}
		return "Optimal " + km(w.Optimal) + " + falloff " + km(w.Falloff)
	case fit.ReachSentry:
		return "Optimal " + km(w.Optimal) + " + falloff " + km(w.Falloff) + ", within " + km(w.Range) + " control range"
	case fit.ReachMissile:
		return "Flight range " + km(w.Range)
	case fit.ReachArea:
		return "Range " + km(w.Optimal)
	case fit.ReachSmartbomb:
		return "Blast radius " + km(w.Range)
	case fit.ReachDrone:
		return "Within " + km(w.Range) + " control range"
	case fit.ReachFighter:
		return "Fly to the target"
	}
	return ""
}

// fitRangeSpark draws the total line as polyline points in a 120 x 28
// box, peak at the top.
func fitRangeSpark(xs, ys []float64, endKm float64) string {
	peak := 0.0
	for _, v := range ys {
		peak = math.Max(peak, v)
	}
	if peak <= 0 || endKm <= 0 {
		return ""
	}
	var b strings.Builder
	for i, km := range xs {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", km/endKm*120, 27-ys[i]/peak*25)
	}
	return b.String()
}

// fitNiceStep rounds a gridline step up to 1, 2, 2.5 or 5 times a
// power of ten (the chart's own gridlines use the same steps).
func fitNiceStep(km float64) float64 {
	if km <= 0 {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(km)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if km <= m*p+1e-9 {
			return m * p
		}
	}
	return 10 * p
}

func dedupeKm(xs []float64) []float64 {
	out := xs[:0]
	for i, v := range xs {
		v = math.Round(v*1000) / 1000
		if i > 0 && len(out) > 0 && v == out[len(out)-1] {
			continue
		}
		out = append(out, v)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
