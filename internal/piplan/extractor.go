package piplan

import "math"

// The extractor output curve, as the game computes it (CCP's
// developer documentation, "Planetary Industry: extraction
// calculation"). qtyPerCycle is ESI's qty_per_cycle: the program's
// base value per 15 minutes for the whole extractor control unit.
// Output falls with time (decay) and wobbles (noise).
const (
	extractorDecay = 0.012 // dogma attribute 1683
	extractorNoise = 0.8   // dogma attribute 1687
)

// ExtractorCycle is the amount one cycle of a program yields.
// cycle counts from 0.
func ExtractorCycle(qtyPerCycle float64, cycleSec int64, cycle int) int64 {
	barWidth := float64(cycleSec) / 900
	t := (float64(cycle) + 0.5) * barWidth
	decay := qtyPerCycle / (1 + t*extractorDecay)
	phase := math.Pow(qtyPerCycle, 0.7)
	a := math.Cos(phase + t/12)
	b := math.Cos(phase/2 + t*0.2)
	c := math.Cos(t * 0.5)
	wobble := math.Max((a+b+c)/3, 0)
	out := barWidth * decay * (1 + extractorNoise*wobble)
	if out == math.Trunc(out) && out > 0 {
		return int64(out) - 1 // the game rounds a whole number down by one
	}
	return int64(out)
}

// ExtractorProgram is every cycle's output over a program.
func ExtractorProgram(qtyPerCycle float64, cycleSec int64, hours float64) []int64 {
	if cycleSec <= 0 || hours <= 0 {
		return nil
	}
	n := int(hours * 3600 / float64(cycleSec))
	out := make([]int64, n)
	for i := range out {
		out[i] = ExtractorCycle(qtyPerCycle, cycleSec, i)
	}
	return out
}

// ProgramCycleSec is the cycle length the game gives a program of
// the given length: 15 minutes under 25 hours, then doubling at 25,
// 50, 100 and 200 hours (four hours at most, for programs up to the
// 336-hour limit).
func ProgramCycleSec(hours float64) int64 {
	if hours < 25 {
		return 900
	}
	steps := min(int(math.Floor(math.Log2(hours/25)))+1, 4)
	return 900 << steps
}

// ProgramLengths are the program lengths the planner offers, in
// hours, matched to how often a player resets extractors.
var ProgramLengths = []float64{24, 48, 72, 96, 120, 168, 240, 336}

// ProgramAverage is the average output per hour of an extractor
// whose peak base is qtyPerCycle (per 15 minutes) over a program of
// the given length.
func ProgramAverage(qtyPerCycle, hours float64) float64 {
	cyc := ProgramCycleSec(hours)
	var sum int64
	for _, v := range ExtractorProgram(qtyPerCycle, cyc, hours) {
		sum += v
	}
	if hours <= 0 {
		return 0
	}
	return float64(sum) / hours
}

// PeakPerHeadHour is what a measured extractor says about its spot on
// the planet: the program's base rate per hour, per head. ESI gives
// qty_per_cycle as the base per 15 minutes for the whole unit.
func PeakPerHeadHour(qtyPerCycle float64, heads int) float64 {
	if heads <= 0 {
		return 0
	}
	return qtyPerCycle * 4 / float64(heads)
}

// QtyPerCycleFor turns a per-head peak hourly rate back into the
// program's base value for a unit with the given heads.
func QtyPerCycleFor(peakPerHeadHour float64, heads int) float64 {
	return peakPerHeadHour * float64(heads) / 4
}

// ECUOutputPerHour is the average hourly output of one extractor
// control unit with the given heads at a per-head peak rate, over a
// program of the given length.
func ECUOutputPerHour(peakPerHeadHour float64, heads int, programHours float64) float64 {
	if heads <= 0 || peakPerHeadHour <= 0 {
		return 0
	}
	return ProgramAverage(QtyPerCycleFor(peakPerHeadHour, heads), programHours)
}
