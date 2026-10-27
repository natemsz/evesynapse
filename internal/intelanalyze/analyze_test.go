package intelanalyze

import (
	"reflect"
	"strings"
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
		"ibis":     {601, "Ibis", ClassOther},
		"velator":  {606, "Velator", ClassOther},
		"gila":     {17715, "Gila", ClassOther},
		"venture":  {32880, "Venture", ClassOther},
		"golem":    {28710, "Golem", ClassOther},
		"mackinaw": {22548, "Mackinaw", ClassOther},
		"hulk":     {22544, "Hulk", ClassOther},
	}
	// Some of these pilots share their names with things and
	// places, as EVE's do: characters called "Force Field",
	// "Station" and "VIII" all exist.
	pilots := map[string]int64{"hostile one": 91000001, "hostile two": 91000002, "solo": 91000003, "friendly guy": 91000004,
		"force field": 91000005, "viii": 91000006, "station": 91000007, "moon": 91000008,
		"minmatar control tower": 91000009, "minmatar military": 91000010, "vherokior": 91000011, "treasury": 91000012,
		"blackcatrus": 91000013, "legendmysticdragon": 91000014, "grid": 91000015, "unclassified": 91000016,
		"wwwlening": 91000017, "hamalias": 91000018, "jito": 91000019, "helid": 91000020}
	types := map[string]int64{"force field": 16103, "minmatar control tower": 16214,
		"raitaru": 35825, "stargate (minmatar system)": 29633}
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
			names := map[int64]string{91000001: "Hostile One", 91000002: "Hostile Two", 91000003: "Solo", 91000004: "Friendly Guy",
				91000013: "BlackCatRus", 91000014: "LegendMysticDragon", 91000017: "WWWLeninG", 91000018: "Hamalias", 91000019: "Jito",
				91000020: "helid"}
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
		{"report scaffolding is not a pilot", Input{System: 1, Text: "Grid"}, KindChatter,
			func(a Analysis) bool { return len(a.Pilots) == 0 && len(a.Unknown) == 0 }},
		{"a standing word is not a pilot", Input{System: 1, Text: "Unclassified"}, KindChatter,
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

// A gate name written with a slip ("Jito" for the Jita gate)
// reads as the gate when the dictionary's near-miss hook knows
// it, and never as the pilot who happens to carry that name:
// EVE has a character called Jito, and in gate position the
// word is the gate. The same holds wherever a line speaks of
// a place: a system named with a slip in a bare location
// ("helid" for Heild, in a line about a ship on a gate) reads
// as the system, though a character carries the slipped
// spelling exactly. Away from place speech the same words
// are the pilots they name, as they should be.
func TestAnalyzeGateNearMiss(t *testing.T) {
	d := dict()
	d.GateNear = func(sys int64, l string) (int64, bool) {
		if sys != 1 {
			return 0, false
		}
		switch l {
		case "jito":
			return 30000142, true
		case "helid":
			return 30002390, true
		}
		return 0, false
	}
	a := Analyze(Input{System: 1, Text: "to Jito gate is", Clear: true}, d)
	if a.Gate != 30000142 {
		t.Errorf("gate %+v, want the Jita gate read from the near-miss", a)
	}
	if len(a.Pilots) != 0 {
		t.Errorf("the gate name became a pilot: %+v", a.Pilots)
	}
	if a.Kind != KindClear {
		t.Errorf("kind %q, want the gate clear", a.Kind)
	}
	b := Analyze(Input{System: 1, Text: "Jito"}, dict())
	if len(b.Pilots) != 1 || b.Pilots[0].Name != "Jito" {
		t.Errorf("away from a gate, Jito is the pilot: %+v", b.Pilots)
	}
	c := Analyze(Input{System: 1, Text: "ship on gate in helid", Hostiles: 1}, d)
	if c.Gate != 30002390 {
		t.Errorf("location %+v, want Heild read from the near-miss", c)
	}
	if len(c.Pilots) != 0 {
		t.Errorf("the location became a pilot: %+v", c.Pilots)
	}
	if c.Kind != KindSighting {
		t.Errorf("kind %q, want the sighting kept", c.Kind)
	}
	e := Analyze(Input{System: 1, Text: "helid"}, d)
	if len(e.Pilots) != 1 || e.Pilots[0].ID != 91000020 {
		t.Errorf("away from place speech, helid is the pilot: %+v", e.Pilots)
	}
}

// The interpreted sentence a scan report carries is analysed
// like any other report text when the report is stored: its
// pilots and ships must chip, and its own scaffolding -- the
// standings words, the grid clause, the anchor -- must not, even
// though EVE has characters named for the scaffolding too.
func TestAnalyzeScanSentence(t *testing.T) {
	d := dict()
	a := Analyze(Input{System: 1, Text: "At the Moon 1 station in Teonusude: 0 hostiles, 0 friendly, 0 neutral, 2 unclassified ships (Ibis, Velator), 0 on grid — BlackCatRus in the Ibis, LegendMysticDragon in the Velator."}, d)
	if len(a.Pilots) != 2 || a.Pilots[0].Name != "BlackCatRus" || a.Pilots[1].Name != "LegendMysticDragon" {
		t.Errorf("pilots %+v, want BlackCatRus and LegendMysticDragon only", a.Pilots)
	}
	shipIDs := map[int64]bool{}
	for _, s := range a.Ships {
		shipIDs[s.TypeID] = true
	}
	if !shipIDs[601] || !shipIDs[606] {
		t.Errorf("ships %+v, want the Ibis and the Velator", a.Ships)
	}
	for _, u := range a.Unknown {
		// The one word a lookup may settle is the system name;
		// phrases the prober forms around it are its normal
		// operation, phrases around anything else are not.
		if !strings.Contains(u, "teonusude") {
			t.Errorf("unknown %q, want phrases around the system name only (%+v)", u, a)
		}
	}
}

// A local report's sentence: counts and the gap clause only, no
// names to chip and nothing left to look up.
func TestAnalyzeLocalSentence(t *testing.T) {
	d := dict()
	a := Analyze(Input{System: 1, Text: "27 in local: 6 hostiles, 4 friendly (incl. you), 17 neutral — no scan attached, ships unknown."}, d)
	if len(a.Pilots) != 0 || len(a.Ships) != 0 || len(a.Unknown) != 0 {
		t.Errorf("%+v, want no pilots, ships or unknowns", a)
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

// TestAnalyzeResidue pins the residue capture: text the
// analysis settles as nothing it knows (looked up, and no
// pilot, ship, gate or keyword) is kept as residue spans,
// exactly the words unread; text it reads whole keeps none.
// What is still waiting on a lookup stands in Unknown, never
// in Residue.
func TestAnalyzeResidue(t *testing.T) {
	d := dict()
	a := Analyze(Input{System: 1, Text: "Hostile One in sabre, some random words"}, d)
	if len(a.Pilots) != 1 || a.Pilots[0].ID != 91000001 || len(a.Ships) != 1 || a.Ships[0].TypeID != 22456 {
		t.Fatalf("the read itself changed: %+v", a)
	}
	if want := []string{"some random words"}; !reflect.DeepEqual(a.Residue, want) {
		t.Errorf("residue = %q, want %q", a.Residue, want)
	}

	full := Analyze(Input{System: 1, Text: "Hostile One in sabre"}, d)
	if len(full.Residue) != 0 {
		t.Errorf("a fully read report: residue %q, want none", full.Residue)
	}

	pending := Analyze(Input{System: 1, Text: "Hostile One in sabre, never asked name"}, d)
	if len(pending.Unknown) == 0 {
		t.Fatalf("an unasked name should stand unknown: %+v", pending)
	}
	if len(pending.Residue) != 0 {
		t.Errorf("text waiting on a lookup is not residue: %q", pending.Residue)
	}
}

// TestAnalyzeScanBlobNames pins the reading of scan-shaped
// text whose separators died on the way in (a d-scan relayed
// through chat): the rows are read as rows, and a name span
// standing in a row's name column -- a player-set ship or
// structure name -- is never minted as a pilot, an unknown or
// residue, however it shreds. The two blobs are the owner's
// reported cards, reconstructed.
func TestAnalyzeScanBlobNames(t *testing.T) {
	d := dict()
	fragments := []string{"ai's", "logistics", "agency", "perch", "free", "scorp",
		"minmatar", "aeddin", "gulfonodi", "teonusude", "blowjob queen", "blowjob", "queen", "monster", "fb"}
	clean := func(a Analysis) {
		t.Helper()
		for _, u := range append(append([]string{}, a.Unknown...), a.Residue...) {
			for _, f := range fragments {
				if u == f || strings.Contains(u, f) {
					t.Errorf("fragment %q surfaced in %q (%+v)", f, u, a)
				}
			}
		}
	}
	shipIDs := func(a Analysis) map[int64]bool {
		ids := map[int64]bool{}
		for _, s := range a.Ships {
			ids[s.TypeID] = true
		}
		return ids
	}

	blob1 := "17715 fb Gila -35825 - AI's Logistics Agency HQ Raitaru 1,048,258 km35825 Teonusude - Eagle's Perch Raitaru 7.4 AU32880 Venture* — WWWLeninG Venture 2,220 m28710 free Golem -28710 scorp Golem -29633 Aeddin Stargate (Minmatar System) 8.0 AU29633 Gulfonodi Stargate (Minmatar System) 12.4 AU"
	a := Analyze(Input{System: 1, Text: blob1}, d)
	clean(a)
	if len(a.Pilots) != 1 || a.Pilots[0].Name != "WWWLeninG" {
		t.Errorf("blob 1 pilots %+v, want WWWLeninG only", a.Pilots)
	}
	for _, id := range []int64{17715, 35825, 32880, 28710, 29633} {
		if !shipIDs(a)[id] {
			t.Errorf("blob 1 ships %+v, want type %d among them", a.Ships, id)
		}
	}
	if len(a.Unknown) != 0 || len(a.Residue) != 0 {
		t.Errorf("blob 1 left unknown %q residue %q, want none", a.Unknown, a.Residue)
	}

	// The second card: "Blowjob queen" is the Mackinaw's
	// player-set name and "monster" the Hulk's; both stand in
	// name columns, so neither is an entity -- and the pilot
	// fused into the Hulk's name span is not minted from it
	// either.
	blob2 := "35825 Teonusude - AI's Logistics Agency HQ Raitaru 1,048,257 km35825 Teonusude - Eagle's Perch Raitaru 7.4 AU22548 Blowjob queen Mackinaw -22544 monster Hamalias Hulk -22544"
	a = Analyze(Input{System: 1, Text: blob2}, d)
	clean(a)
	if len(a.Pilots) != 0 {
		t.Errorf("blob 2 pilots %+v, want none minted from name columns", a.Pilots)
	}
	for _, id := range []int64{35825, 22548, 22544} {
		if !shipIDs(a)[id] {
			t.Errorf("blob 2 ships %+v, want type %d among them", a.Ships, id)
		}
	}
	if len(a.Unknown) != 0 || len(a.Residue) != 0 {
		t.Errorf("blob 2 left unknown %q residue %q, want none", a.Unknown, a.Residue)
	}

	// An overview's rows read by their tails: the name column
	// there names the pilot, and the tickers and velocity
	// figures are the row's, not entities.
	overview := "Hamalias Hulk 12 km 145 m/s -23 m/s 34 m/s [NAT.] WWWLeninG Venture 900 m 0 m/s 12 m/s 3 m/s"
	a = Analyze(Input{System: 1, Text: overview}, d)
	clean(a)
	if len(a.Pilots) != 2 || a.Pilots[0].Name != "Hamalias" || a.Pilots[1].Name != "WWWLeninG" {
		t.Errorf("overview pilots %+v, want Hamalias and WWWLeninG", a.Pilots)
	}
	if len(a.Unknown) != 0 || len(a.Residue) != 0 {
		t.Errorf("overview left unknown %q residue %q, want none", a.Unknown, a.Residue)
	}

	// A structure name with its type column lost is one
	// location; with the type still standing, the type is read
	// and the name still set aside.
	for _, text := range []string{"Teonusude - Eagle's Perch 7.4 AU", "Teonusude - Eagle's Perch Raitaru 7.4 AU"} {
		a = Analyze(Input{System: 1, Text: text}, d)
		clean(a)
		if len(a.Pilots) != 0 || len(a.Unknown) != 0 || len(a.Residue) != 0 {
			t.Errorf("%q: pilots %+v unknown %q residue %q, want none", text, a.Pilots, a.Unknown, a.Residue)
		}
	}
	if a = Analyze(Input{System: 1, Text: "Teonusude - Eagle's Perch Raitaru 7.4 AU"}, d); !shipIDs(a)[35825] {
		t.Errorf("typed structure: ships %+v, want the Raitaru", a.Ships)
	}
}

// Chat shorthand is never a pilot in free-form text, though EVE
// has characters called "Clr" and "Nv"; a short or everyday word
// that does read as a pilot is kept, and marked suspect.
func TestAnalyzeChatWordsAndSuspects(t *testing.T) {
	d := dict()
	base := d.Pilot
	d.Pilot = func(l string) (int64, string, bool, bool) {
		switch l {
		case "clr":
			return 92000001, "Clr", true, true
		case "nv":
			return 92000002, "Nv", true, true
		case "ess":
			return 92000003, "Ess", true, true
		}
		return base(l)
	}
	a := Analyze(Input{System: 1, Text: "Clr"}, d)
	if len(a.Pilots) != 0 {
		t.Errorf("clr read as a pilot: %+v", a.Pilots)
	}
	a = Analyze(Input{System: 1, Text: "Hostile One  nv"}, d)
	if len(a.Pilots) != 1 || a.Pilots[0].Name != "Hostile One" {
		t.Errorf("nv read as a pilot: %+v", a.Pilots)
	}
	if len(a.Suspect) != 0 {
		t.Errorf("a two-word pilot marked suspect: %q", a.Suspect)
	}
	a = Analyze(Input{System: 1, Text: "Hostile One  Ess"}, d)
	if len(a.Pilots) != 2 {
		t.Fatalf("pilots: %+v", a.Pilots)
	}
	if !reflect.DeepEqual(a.Suspect, []string{"Ess"}) {
		t.Errorf("suspect = %q, want [Ess]", a.Suspect)
	}
}

// The segmentation index never anchors on a name free-form text
// refuses: a fused d-scan's "Station" and "Moon" stay unread.
func TestNameIndexSkipsNonCandidates(t *testing.T) {
	ix := NewNameIndex([]Pilot{{ID: 1, Name: "Station"}, {ID: 2, Name: "Moon"}, {ID: 3, Name: "Clr"}, {ID: 4, Name: "Hamalias"}})
	got, _, _ := ix.Segment("StationMoonClrHamalias")
	if len(got) != 1 || got[0].Name != "Hamalias" {
		t.Errorf("segmented %+v, want only Hamalias", got)
	}
}
