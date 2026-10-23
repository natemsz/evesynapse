package intelanalyze

import (
	"reflect"
	"testing"
)

// A toy dictionary: a few ships, pilots and gates.
func dict() Dict {
	ships := map[string]struct {
		id          int64
		name, class string
	}{
		"sabre":    {22456, "Sabre", ClassInterdict},
		"loki":     {29990, "Loki", ClassHunter},
		"rifter":   {587, "Rifter", ClassOther},
		"nyx":      {23913, "Nyx", ClassCapital},
		"purifier": {12038, "Purifier", ClassBomber},
	}
	// Some of these pilots share their names with things and
	// places, as EVE's do: characters called "Force Field",
	// "Station" and "VIII" all exist.
	pilots := map[string]int64{"hostile one": 91000001, "hostile two": 91000002, "solo": 91000003, "friendly guy": 91000004,
		"force field": 91000005, "viii": 91000006, "station": 91000007, "moon": 91000008,
		"minmatar control tower": 91000009, "minmatar military": 91000010, "vherokior": 91000011, "treasury": 91000012}
	types := map[string]int64{"force field": 16103, "minmatar control tower": 16214}
	asked := map[string]bool{"some random words": true, "some random": true, "random words": true, "some": true, "random": true, "words": true}
	return Dict{
		Ship: func(l string) (int64, string, string, bool) {
			s, ok := ships[l]
			return s.id, s.name, s.class, ok
		},
		Type: func(l string) (int64, string, bool) {
			id, ok := types[l]
			return id, l, ok
		},
		Pilot: func(l string) (int64, string, bool, bool) {
			id, ok := pilots[l]
			names := map[int64]string{91000001: "Hostile One", 91000002: "Hostile Two", 91000003: "Solo", 91000004: "Friendly Guy"}
			return id, names[id], ok, ok || asked[l]
		},
		Gate: func(sys int64, l string) (int64, bool) {
			if sys == 1 && l == "jita" {
				return 30000142, true
			}
			return 0, false
		},
		Friend: func(id int64) (bool, bool) { return id == 91000004, false },
	}
}

func TestAnalyze(t *testing.T) {
	d := dict()
	cases := []struct {
		name string
		in   Input
		kind string
		want func(a Analysis) bool
	}{
		{"names from Local, a count", Input{System: 1, Text: "Hostile One  Hostile Two", Hostiles: 2}, KindSighting,
			func(a Analysis) bool { return len(a.Pilots) == 2 && a.Pilots[1].Name == "Hostile Two" }},
		{"names with one space, found word by word", Input{System: 1, Text: "Hostile One Hostile Two"}, KindSighting,
			func(a Analysis) bool { return len(a.Pilots) == 2 }},
		{"a sabre on the Jita gate is a bubble", Input{System: 1, Text: "Hostile One sabre on Jita gate"}, KindBubble,
			func(a Analysis) bool {
				return a.Gate == 30000142 && len(a.Ships) == 1 && a.Ships[0].TypeID == 22456 && len(a.Pilots) == 1
			}},
		{"a camp, ships counted", Input{System: 1, Text: "gate camp 2x lokis  rifter x3"}, KindCamp,
			func(a Analysis) bool {
				return len(a.Ships) == 2 && a.Ships[0].N == 2 && a.Ships[1].Name == "Rifter" && a.Ships[1].N == 3
			}},
		{"a cyno is capitals", Input{System: 1, Text: "cyno up"}, KindCapital, nil},
		{"a Nyx is capitals", Input{System: 1, Text: "nyx"}, KindCapital, nil},
		{"bombers are a fleet", Input{System: 1, Text: "purifiers"}, KindFleet, nil},
		{"fifteen is a fleet", Input{System: 1, Hostiles: 15}, KindFleet, nil},
		{"an inty by its nickname", Input{System: 1, Text: "inty"}, KindSighting,
			func(a Analysis) bool { return len(a.Ships) == 1 && a.Ships[0].Class == ClassFast }},
		{"neut in local, no visual", Input{System: 1, Text: "neut", NoVisual: true}, KindLocal, nil},
		{"only a friendly", Input{System: 1, Text: "Friendly Guy"}, KindFriendly,
			func(a Analysis) bool { return a.Pilots[0].Friendly }},
		{"a friendly and a hostile is a sighting", Input{System: 1, Text: "Friendly Guy  Solo"}, KindSighting, nil},
		{"clear", Input{System: 1, Clear: true}, KindClear, nil},
		{"a type, not the character with its name", Input{System: 1, Text: "Force Field"}, KindSighting,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Ships) == 1 && a.Ships[0].TypeID == 16103 }},
		{"a control tower is a type, not a pilot", Input{System: 1, Text: "Minmatar Control Tower"}, KindSighting,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Ships) == 1 && a.Ships[0].TypeID == 16214 }},
		{"a pilot and a type together", Input{System: 1, Text: "Hostile One  Force Field"}, KindSighting,
			func(a Analysis) bool { return len(a.Pilots) == 1 && len(a.Ships) == 1 && a.Ships[0].TypeID == 16103 }},
		{"a roman numeral is not a pilot", Input{System: 1, Text: "VIII"}, KindChatter,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Unknown) == 0 }},
		{"a bare place noun is not a pilot", Input{System: 1, Text: "Station"}, KindChatter,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Unknown) == 0 }},
		{"a station's name is one location, not pilots", Input{System: 1, Text: "VII - Moon 1 - Vherokior Tribe Treasury Minmatar Military Station"}, KindChatter,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Unknown) == 0 && len(a.Ships) == 0 }},
		{"a mangled d-scan line: types and a location, no pilots", Input{System: 1, Text: "16103 Force Field Minmatar Control Tower 4,875 km 2498 VII - Moon 1 - Vherokior Tribe Treasury Minmatar Military Station 0 m"}, KindSighting,
			func(a Analysis) bool {
				if len(a.Pilots) != 0 || len(a.Unknown) != 0 || len(a.Ships) != 2 {
					return false
				}
				ids := map[int64]bool{a.Ships[0].TypeID: true, a.Ships[1].TypeID: true}
				return ids[16103] && ids[16214]
			}},
		{"words no one knows yet", Input{System: 1, Text: "Unheard Of"}, KindSighting,
			func(a Analysis) bool { return reflect.DeepEqual(a.Unknown, []string{"unheard of", "unheard", "of"}) }},
		{"words known to be no one", Input{System: 1, Text: "some random words"}, KindChatter,
			func(a Analysis) bool { return len(a.Unknown) == 0 }},
	}
	for _, c := range cases {
		a := Analyze(c.in, d)
		if a.Kind != c.kind {
			t.Errorf("%s: kind %q, want %q (%+v)", c.name, a.Kind, c.kind, a)
			continue
		}
		if c.want != nil && !c.want(a) {
			t.Errorf("%s: %+v", c.name, a)
		}
	}
}

func TestRank(t *testing.T) {
	order := []string{KindCapital, KindCamp, KindBubble, KindFleet, KindSighting, KindLocal, KindFriendly, KindClear, KindQuestion, KindChatter}
	for i := 1; i < len(order); i++ {
		if Rank(order[i-1]) >= Rank(order[i]) {
			t.Errorf("%s should rank before %s", order[i-1], order[i])
		}
	}
}
