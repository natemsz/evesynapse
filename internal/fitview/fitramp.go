package fitview

import (
	"encoding/json"
	"math"
	"sort"

	"evesynapse/internal/fit"
)

// fitRampSamples is how many even steps the time axis takes; every
// cycle boundary adds its own pair of points on top, so a bonus step
// draws as a step.
const fitRampSamples = 120

// FitRampData is the figures-over-time chart's data: the time axis
// in seconds and one chart per ramping output (damage, remote armor).
type FitRampData struct {
	End    float64        `json:"end"`
	T      []float64      `json:"t"`
	Charts []FitRampChart `json:"charts"`
}

// FitRampChart is one output over time: its rate at each moment of
// continuous firing (Now) and its average since the first cycle
// began (Avg), with the start and full rates and when it is full.
type FitRampChart struct {
	Key    string    `json:"key"`
	Title  string    `json:"title"`
	Unit   string    `json:"unit"`
	Start  float64   `json:"start"`
	Full   float64   `json:"full"`
	FullAt float64   `json:"fullAt"`
	Now    []float64 `json:"now"`
	Avg    []float64 `json:"avg"`
}

// fitRampOutput is one ramping output: the rate of everything that
// does not ramp, plus the ramps.
type fitRampOutput struct {
	kind  fit.RampKind
	base  float64 // the whole output's rate at the start
	ramps []fit.Ramp
}

func (o fitRampOutput) fullAt() float64 {
	t := 0.0
	for _, r := range o.ramps {
		t = math.Max(t, r.FullAfter())
	}
	return t
}

func (o fitRampOutput) now(t float64) float64 {
	v := o.base
	for _, r := range o.ramps {
		v += r.RateAt(t) - r.RateAt(0)
	}
	return v
}

// avg is the output's average rate over its first t seconds.
func (o fitRampOutput) avg(t float64) float64 {
	if t <= 0 {
		return o.base
	}
	v := o.base
	for _, r := range o.ramps {
		v += r.OutputBy(t)/t - r.RateAt(0)
	}
	return v
}

// FitRampAverage is the output's average rate from the first cycle
// until it reaches full bonus: what a fight that long actually sees.
func fitRampAverage(o fitRampOutput) float64 { return o.avg(o.fullAt()) }

// fitRampOutputs splits a fit's ramps by what they put out.
func fitRampOutputs(res *fit.Result) []fitRampOutput {
	var dmg, armor fitRampOutput
	dmg.kind, dmg.base = fit.RampDamage, res.DPS
	armor.kind, armor.base = fit.RampArmor, res.RemoteArmorRate
	for _, r := range res.Ramps {
		switch r.Kind {
		case fit.RampDamage:
			dmg.ramps = append(dmg.ramps, r)
		case fit.RampArmor:
			armor.ramps = append(armor.ramps, r)
		}
	}
	var out []fitRampOutput
	for _, o := range []fitRampOutput{dmg, armor} {
		if len(o.ramps) > 0 && o.fullAt() > 0 {
			out = append(out, o)
		}
	}
	return out
}

// FitRamp builds the figures-over-time chart's data, its JSON and the
// inline sparkline's points (the first chart's rate, 120 x 28 box).
// All three are empty for a fit with nothing that ramps.
func FitRamp(res *fit.Result) (data *FitRampData, js, spark string) {
	if res == nil {
		return nil, "", ""
	}
	outs := fitRampOutputs(res)
	if len(outs) == 0 {
		return nil, "", ""
	}
	full := 0.0
	for _, o := range outs {
		full = math.Max(full, o.fullAt())
	}
	// The axis runs a quarter past the last ramp's full point, out to
	// the next gridline, so the flat full-bonus stretch shows.
	step := fitTimeStep(full * 1.25)
	end := math.Ceil(full*1.25/step) * step

	ts := make([]float64, 0, fitRampSamples+64)
	for i := 0; i <= fitRampSamples; i++ {
		ts = append(ts, end*float64(i)/fitRampSamples)
	}
	for _, o := range outs {
		for _, r := range o.ramps {
			if r.Cycle <= 0 {
				continue
			}
			for b := r.Cycle; b <= math.Min(r.FullAfter(), end)+1e-9; b += r.Cycle {
				ts = append(ts, b-0.01, b)
			}
		}
	}
	sort.Float64s(ts)
	ts = dedupeKm(ts)

	data = &FitRampData{End: end, T: ts}
	for _, o := range outs {
		c := FitRampChart{Key: string(o.kind), Start: round1(o.base), Full: round1(o.now(o.fullAt())), FullAt: o.fullAt()}
		switch o.kind {
		case fit.RampDamage:
			c.Title, c.Unit = "Damage over time", "DPS"
		case fit.RampArmor:
			c.Title, c.Unit = "Remote armor repair over time", "HP/s"
		}
		c.Now = make([]float64, len(ts))
		c.Avg = make([]float64, len(ts))
		for i, t := range ts {
			c.Now[i] = round1(o.now(t))
			c.Avg[i] = round1(o.avg(t))
		}
		data.Charts = append(data.Charts, c)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, "", ""
	}
	return data, string(b), fitRangeSpark(ts, data.Charts[0].Now, end)
}

// fitTimeStep is the time axis's gridline step for a span of seconds:
// the steps a clock reads, a quarter of the span or more (fitramp.js
// picks the same).
func fitTimeStep(span float64) float64 {
	for _, st := range []float64{5, 10, 15, 30, 60, 120, 300, 600} {
		if span/4 <= st {
			return st
		}
	}
	return 600
}
