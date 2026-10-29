package intelanalyze_test

import (
	"hash/fnv"
	"reflect"
	"strings"
	"testing"
	"time"

	"evesynapse/internal/intelanalyze"
	"evesynapse/internal/intelchat"
)

// TestProseReadings reads scouts' sentences against the worst
// name lookup there is, and the one ESI comes close to: every
// string is some character. What reads as a pilot must then come
// from the text's own shape, never from the lookup saying yes.
func TestProseReadings(t *testing.T) {
	sys := map[string]int64{"1dq1-a": 1, "nol-m9": 2, "6vdt-h": 3, "jita": 4, "b-vip9": 5, "4o-239": 6,
		"3-dmqt": 7, "bosena": 8, "helid": 9, "ww-kgd": 10, "perimeter": 12}
	nb := map[int64][]int64{1: {2}, 2: {1}, 7: {6}, 9: {8}, 8: {9}, 4: {12}}
	ships := map[string]string{"harpy": "", "ikitursa": "", "keres": "", "sacrilege": "", "hurricane": ""}
	d := intelanalyze.Dict{
		Ship: func(l string) (int64, string, string, bool) {
			c, ok := ships[l]
			return 1, l, c, ok
		},
		Pilot:  func(l string) (int64, string, bool, bool) { return nameID(l), l, true, true },
		System: func(l string) (int64, bool) { id, ok := sys[l]; return id, ok },
		Area:   func(l string) (int64, bool) { return 10000060, l == "delve" },
		Gate: func(s int64, l string) (int64, bool) {
			id := sys[l]
			for _, n := range nb[s] {
				if n == id {
					return id, true
				}
			}
			return 0, false
		},
	}
	cases := []struct {
		line   string
		system int64
		pilots []string
		gate   int64
		loc    string
	}{
		{"1DQ1-A Sort Dragon in a harpy on NOL-M9 gate", 1, []string{"sort dragon"}, 2, ""},
		{"6VDT-H +1 neut on moon 4", 3, nil, 0, "Moon 4"},
		{"Jita 4-4 undock camped", 4, nil, 0, "Planet 4 Moon 4"},
		{"Jita help, tackled on perimeter gate", 4, nil, 12, ""},
		{"NOL-M9 -> 1DQ1-A +31 fleet", 2, nil, 1, ""},
		{"Dof* logged off in B-VIP9, local clr", 5, []string{"dof"}, 0, ""},
		{"3-DMQT bubble up on 4O-239 gate", 7, nil, 6, ""},
		{"Jita wreck and container on scan, no ships", 4, nil, 0, ""},
		{"Jita P3M2 ihub reinforced", 4, nil, 0, "Planet 3 Moon 2"},
		{"1 hostile ship on bosena gate in helid", 9, nil, 8, ""},
		{"Jita he went to perimeter", 4, nil, 12, ""},
		{"Jita red in local, cloaky, probably hunting", 4, nil, 0, ""},
		{"3-DMQT Calebyr* ikitursa on dscan", 7, []string{"calebyr"}, 0, ""},
		{"Jita spike in local, 12 neuts, looks like a roam", 4, nil, 0, ""},
		{"Jita still there", 4, nil, 0, ""},
		{"Jita camping the sun", 4, nil, 0, "Sun"},
		{"Jita gang of 20 heading your way", 4, nil, 0, ""},
		{"Jita moon 3 is being mined by hostiles", 4, nil, 0, "Moon 3"},
		{"Jita Hostile One  Hostile Two", 4, []string{"hostile one", "hostile two"}, 0, ""},
		{"Jita 5 sacs and a cane on perimeter gate", 4, nil, 12, ""},
		{"Jita guristas in belt 3", 4, nil, 0, "Belt 3"},
		{"Jita gang heading to delve", 4, nil, 0, ""},
	}
	lookup := func(l string) (int64, string, bool) { id, ok := sys[l]; return id, l, ok }
	for _, c := range cases {
		out := intelchat.Parse(c.line, lookup, time.Now())
		if len(out.Reports) != 1 {
			t.Errorf("%q: %d reports", c.line, len(out.Reports))
			continue
		}
		r := out.Reports[0]
		if r.SystemID != c.system {
			t.Errorf("%q: system %d, want %d", c.line, r.SystemID, c.system)
		}
		a := intelanalyze.Analyze(intelanalyze.Input{System: r.SystemID, Text: r.Text, Hostiles: r.Hostiles, Clear: r.Clear, NoVisual: r.NoVisual}, d)
		var got []string
		for _, p := range a.Pilots {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, c.pilots) {
			t.Errorf("%q: pilots %q, want %q", c.line, got, c.pilots)
		}
		if a.Gate != c.gate {
			t.Errorf("%q: gate %d, want %d", c.line, a.Gate, c.gate)
		}
		if a.Location != c.loc {
			t.Errorf("%q: location %q, want %q", c.line, a.Location, c.loc)
		}
		if strings.Contains(c.line, "wreck") && len(a.Unknown) > 0 {
			t.Errorf("%q: offered %q to the lookup", c.line, a.Unknown)
		}
	}
}

// A system named that is not a neighbour is a place spoken of.
func TestProseOtherSystem(t *testing.T) {
	d := intelanalyze.Dict{
		Pilot:  func(l string) (int64, string, bool, bool) { return 900, l, true, true },
		System: func(l string) (int64, bool) { return 77, l == "amamake" },
		Gate:   func(int64, string) (int64, bool) { return 0, false },
	}
	a := intelanalyze.Analyze(intelanalyze.Input{System: 1, Text: "gang came from amamake"}, d)
	if len(a.Pilots) != 0 || !reflect.DeepEqual(a.Systems, []int64{77}) {
		t.Errorf("pilots %+v systems %v, want none and [77]", a.Pilots, a.Systems)
	}
}

// nameID is a stable made-up character ID for a name.
func nameID(l string) int64 {
	h := fnv.New32a()
	h.Write([]byte(l))
	return int64(h.Sum32()) + 90000000
}

// Nicknames read as the hull, NPC rats as rats, a region as a place.
func TestProseShipsRatsAreas(t *testing.T) {
	d := intelanalyze.Dict{
		Ship: func(l string) (int64, string, string, bool) {
			return 12019, "Sacrilege", "", l == "sacrilege"
		},
		Pilot: func(l string) (int64, string, bool, bool) { return nameID(l), l, true, true },
		Area:  func(l string) (int64, bool) { return 10000060, l == "delve" },
	}
	a := intelanalyze.Analyze(intelanalyze.Input{System: 1, Text: "3 sacs, guristas, came from Delve"}, d)
	if len(a.Pilots) != 0 {
		t.Errorf("pilots %+v, want none", a.Pilots)
	}
	if len(a.Ships) != 1 || a.Ships[0].Name != "Sacrilege" || a.Ships[0].N != 3 {
		t.Errorf("ships %+v, want 3 Sacrilege", a.Ships)
	}
	if !reflect.DeepEqual(a.Areas, []int64{10000060}) {
		t.Errorf("areas %v, want [10000060]", a.Areas)
	}
	if !reflect.DeepEqual(a.Flags, []string{"npc"}) {
		t.Errorf("flags %q, want [npc]", a.Flags)
	}
}
