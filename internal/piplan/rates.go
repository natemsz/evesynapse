package piplan

import "math"

// Relink lays a colony's links and routes again around its first
// launchpad, after the player moved, added or removed structures.
func Relink(c *Colony) { linkAndRoute(c) }

// ColonyRates is what a colony sends out and takes in per hour, once
// it uses what it makes itself. Extractors run at their program's
// average; basic factories turn as much raw as they can take;
// advanced and high-tech factories are counted at full speed, fed
// from outside for what the colony does not make.
func ColonyRates(c *Colony) (out, in map[string]float64) {
	made := map[string]float64{}
	used := map[string]float64{}
	raw := map[string]float64{}
	basics := map[string]int{}
	for _, p := range c.Pins {
		if p.Keep == ChangeRemove {
			continue
		}
		switch p.Kind {
		case PinExtractor:
			if p.Product != "" {
				hrs := p.ProgramHours
				if hrs <= 0 {
					hrs = 24
				}
				raw[p.Product] += ECUOutputPerHour(peakFor(p), p.Heads, hrs)
			}
		case PinBasic:
			basics[p.Product]++
		case PinAdvanced, PinHighTech:
			s, ok := Schematics[p.Product]
			if !ok {
				continue
			}
			rate := 3600 / float64(s.CycleSec)
			made[p.Product] += rate * float64(s.OutputQty)
			for inp, per := range s.Inputs {
				used[inp] += rate * float64(per)
			}
		}
	}
	fillP1Lazy()
	// Basic factories: raw from the planet first, the rest imported.
	for p1, n := range basics {
		s, ok := Schematics[p1]
		if !ok {
			continue
		}
		var rawName string
		for in := range s.Inputs {
			rawName = in
		}
		capRaw := float64(n) * basicRawPerHour
		have := raw[rawName]
		take := math.Min(have, capRaw)
		raw[rawName] -= take
		made[p1] += take / basicRawPerHour * basicP1PerHour
	}
	out, in = map[string]float64{}, map[string]float64{}
	for k, v := range made {
		net := v - used[k]
		if net > 1e-6 {
			out[k] = net
		} else if net < -1e-6 {
			in[k] = -net
		}
	}
	for k, v := range used {
		if _, mk := made[k]; !mk && v > 1e-6 {
			in[k] = v
		}
	}
	for k, v := range raw {
		if v > 1e-6 {
			out[k] += v // unrefined raw: better refined, but it can leave
		}
	}
	return out, in
}

func fillP1Lazy() { p1Once.Do(fillP1ForRaw) }
