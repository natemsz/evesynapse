package intelanalyze

import "sort"

// Report is one analysed report, as Situations takes it.
type Report struct {
	ID       int64
	System   int64
	At       int64 // unix seconds
	Reporter string
	Hostiles int
	Clear    bool
	Kind     string
	Analysis Analysis
}

// Situation is what is going on in one system now: every report there
// since its last clear, taken together.
type Situation struct {
	System    int64    `json:"sys"`
	Kind      string   `json:"kind"` // the most pressing kind reported
	Pilots    []Pilot  `json:"pilots,omitempty"`
	Ships     []Ship   `json:"ships,omitempty"`
	Count     int      `json:"count,omitempty"` // the largest count said
	Gate      int64    `json:"gate,omitempty"`
	From      int64    `json:"from,omitempty"` // a neighbour its pilots were seen in just before
	Reporters int      `json:"reporters"`
	First     int64    `json:"first"`
	Last      int64    `json:"last"`
	Flags     []string `json:"flags,omitempty"`
	Reports   []int64  `json:"reports"`
}

// Calm reports whether the situation is nothing to worry about: a
// clear, or friendlies only.
func (s Situation) Calm() bool {
	return s.Kind == KindClear || s.Kind == KindFriendly
}

// Situations takes reports (any order) together by system. A clear,
// or a report of friendlies only, newer than a system's hostile
// reports leaves it calm; reports before the last clear are left out.
// Chatter and questions are not reports of anything. A pilot seen in
// a neighbouring system shortly before (within window seconds) marks
// where the pilots came from. Sorted most pressing first, then newest.
func Situations(reports []Report, adjacent func(a, b int64) bool, window int64) []Situation {
	rs := make([]Report, 0, len(reports))
	for _, r := range reports {
		if r.Kind == KindChatter || r.Kind == KindQuestion {
			continue
		}
		rs = append(rs, r)
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].At != rs[j].At {
			return rs[i].At < rs[j].At
		}
		return rs[i].ID < rs[j].ID
	})
	bySys := map[int64][]Report{}
	var order []int64
	for _, r := range rs {
		if _, ok := bySys[r.System]; !ok {
			order = append(order, r.System)
		}
		bySys[r.System] = append(bySys[r.System], r)
	}
	// Where each pilot was seen, oldest first, for movement.
	type seen struct {
		sys, at int64
	}
	sightings := map[int64][]seen{}
	for _, r := range rs {
		if r.Clear {
			continue
		}
		for _, p := range r.Analysis.Pilots {
			if !p.Friendly || p.Red {
				sightings[p.ID] = append(sightings[p.ID], seen{r.System, r.At})
			}
		}
	}
	var out []Situation
	for _, sys := range order {
		list := bySys[sys]
		// Only what came after the last calm report counts.
		start := 0
		for i, r := range list {
			if r.Clear || r.Kind == KindClear || r.Kind == KindFriendly {
				start = i
			}
		}
		s := Situation{System: sys, First: list[start].At, Kind: list[start].Kind}
		if s.Kind == "" {
			s.Kind = KindSighting
		}
		reporters := map[string]bool{}
		pilotAt, shipAt, flags := map[int64]int{}, map[string]int{}, map[string]bool{}
		for _, r := range list[start:] {
			s.Reports = append(s.Reports, r.ID)
			s.Last = r.At
			if r.Reporter != "" {
				reporters[r.Reporter] = true
			}
			if r.Kind != "" && Rank(r.Kind) < Rank(s.Kind) {
				s.Kind = r.Kind
			}
			if r.Hostiles > s.Count {
				s.Count = r.Hostiles
			}
			a := r.Analysis
			if a.Gate > 0 {
				s.Gate = a.Gate
			}
			for _, f := range a.Flags {
				flags[f] = true
			}
			for _, p := range a.Pilots {
				if i, ok := pilotAt[p.ID]; ok {
					s.Pilots[i] = p
					continue
				}
				pilotAt[p.ID] = len(s.Pilots)
				s.Pilots = append(s.Pilots, p)
			}
			for _, sh := range a.Ships {
				if i, ok := shipAt[sh.Name]; ok {
					if sh.N > s.Ships[i].N {
						s.Ships[i].N = sh.N
					}
					continue
				}
				shipAt[sh.Name] = len(s.Ships)
				s.Ships = append(s.Ships, sh)
			}
		}
		// A calm report as the newest leaves the system calm.
		if last := list[len(list)-1]; last.Clear || last.Kind == KindClear || last.Kind == KindFriendly {
			s.Kind = last.Kind
			if last.Clear {
				s.Kind = KindClear
			}
		}
		s.Reporters = len(reporters)
		for f := range flags {
			s.Flags = append(s.Flags, f)
		}
		sort.Strings(s.Flags)
		// Where its hostile pilots came from: the latest neighbouring
		// system any of them was seen in before.
		if adjacent != nil && !s.Calm() {
			bestAt := int64(0)
			for _, p := range s.Pilots {
				if p.Friendly && !p.Red {
					continue
				}
				for _, sg := range sightings[p.ID] {
					if sg.sys != sys && sg.at <= s.Last && sg.at >= s.First-window && sg.at > bestAt && adjacent(sg.sys, sys) {
						bestAt, s.From = sg.at, sg.sys
					}
				}
			}
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if Rank(out[i].Kind) != Rank(out[j].Kind) {
			return Rank(out[i].Kind) < Rank(out[j].Kind)
		}
		return out[i].Last > out[j].Last
	})
	return out
}
