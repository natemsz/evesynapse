package piplan

import "sort"

// OptimizeLayout moves a colony's movable structures to the places
// where its links cost the least CPU and power. The busiest
// structures go nearest the launchpad hub first, then single moves
// and swaps are tried until none helps. Structures the colony already
// has (kept, retooled or being removed), the hub and the command
// center stay where they stand. The colony keeps its old layout when
// no arrangement beats it. True when the layout changed.
func OptimizeLayout(c *Colony) bool {
	linkAndRoute(c)
	hub := -1
	for i, p := range c.Pins {
		if p.Keep != ChangeRemove && p.Kind == PinLaunchpad {
			hub = i
			break
		}
	}
	if hub < 0 {
		return false
	}
	var movable []int
	isMovable := map[int]bool{}
	for i, p := range c.Pins {
		if i != hub && p.Kind != PinCommandCenter && (p.Keep == "" || p.Keep == ChangeAdd) {
			movable = append(movable, i)
			isMovable[i] = true
		}
	}
	// Real colonies hold a few dozen structures; past that the search
	// is not worth its time.
	if len(movable) == 0 || len(movable) > 48 {
		return false
	}
	orig := make([]LatLon, len(c.Pins))
	for i, p := range c.Pins {
		orig[i] = p.pos()
	}
	cost := func() float64 {
		linkAndRoute(c)
		f := c.Fit()
		v := 0.0
		if f.PGCap > 0 {
			v += float64(f.PGUsed) / float64(f.PGCap)
		}
		if f.CPUCap > 0 {
			v += float64(f.CPUUsed) / float64(f.CPUCap)
		}
		km := 0.0
		for _, l := range c.Links {
			km += c.LinkKm(l)
		}
		return v + km*1e-9 // shorter links break ties
	}
	start := cost()

	// Cells around the hub clear of every structure that stays put,
	// nearest first, with room to spare for moves.
	center := c.Pins[hub].pos()
	var cells []LatLon
	for rings := 2; rings <= 8; rings++ {
		cells = cells[:0]
		for _, h := range hexCells(rings) {
			if h.ring == 0 {
				continue
			}
			pos := cellPos(center, h)
			clear := true
			for i, p := range c.Pins {
				if !isMovable[i] && Angle(p.pos(), pos) < gridStep*0.9 {
					clear = false
					break
				}
			}
			if clear {
				cells = append(cells, pos)
			}
		}
		if len(cells) >= len(movable)+6 {
			break
		}
	}
	cells = cells[:min(len(cells), len(movable)+6)]
	if len(cells) < len(movable) {
		return false
	}

	// Busiest first, onto the nearest cells.
	traffic := map[int]float64{}
	for _, i := range movable {
		traffic[i] = pinTraffic(c.Pins[i])
	}
	order := append([]int(nil), movable...)
	sort.SliceStable(order, func(a, b int) bool { return traffic[order[a]] > traffic[order[b]] })
	at := map[int]int{}   // pin -> cell
	held := map[int]int{} // cell -> pin
	place := func(i, k int) {
		p := &c.Pins[i]
		pos := cells[k]
		dLat, dLon := pos.Lat-p.Lat, pos.Lon-p.Lon
		for h := range p.HeadPos {
			p.HeadPos[h].Lat += dLat
			p.HeadPos[h].Lon += dLon
		}
		p.Lat, p.Lon = pos.Lat, pos.Lon
		at[i] = k
		held[k] = i
	}
	for n, i := range order {
		place(i, n)
	}
	best := cost()

	// Moves and swaps while any helps.
	for range 4 {
		improved := false
		for _, i := range order {
			for k := range cells {
				from := at[i]
				if k == from {
					continue
				}
				other, taken := held[k]
				delete(held, from)
				place(i, k)
				if taken {
					place(other, from)
				}
				if v := cost(); v < best-1e-12 {
					best, improved = v, true
					continue
				}
				// Undo.
				delete(held, k)
				place(i, from)
				if taken {
					place(other, k)
				}
			}
		}
		if !improved {
			break
		}
	}
	if best >= start-1e-12 {
		for i := range c.Pins {
			p := &c.Pins[i]
			dLat, dLon := orig[i].Lat-p.Lat, orig[i].Lon-p.Lon
			for h := range p.HeadPos {
				p.HeadPos[h].Lat += dLat
				p.HeadPos[h].Lon += dLon
			}
			p.Lat, p.Lon = orig[i].Lat, orig[i].Lon
		}
		linkAndRoute(c)
		return false
	}
	linkAndRoute(c)
	return true
}

// pinTraffic is the volume a structure sends and takes in per hour:
// what its link toward the hub carries for it.
func pinTraffic(p Pin) float64 {
	vol := func(name string) float64 { return AllCommodities[name].Volume }
	switch p.Kind {
	case PinExtractor:
		if p.Product == "" {
			return 0
		}
		return ECUOutputPerHour(peakFor(p), p.Heads, p.ProgramHours) * vol(p.Product)
	case PinBasic, PinAdvanced, PinHighTech:
		sch, ok := Schematics[p.Product]
		if !ok {
			return 0
		}
		perHour := 3600 / float64(sch.CycleSec)
		t := float64(sch.OutputQty) * perHour * vol(p.Product)
		for n, q := range sch.Inputs {
			t += float64(q) * perHour * vol(n)
		}
		return t
	}
	return 0
}
