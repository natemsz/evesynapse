package intelanalyze

import "testing"

func TestSituations(t *testing.T) {
	hostile := func(id int64, name string) Pilot { return Pilot{ID: id, Name: name} }
	adj := func(a, b int64) bool { return (a == 1 && b == 2) || (a == 2 && b == 1) }
	reports := []Report{
		// System 1: a sighting, then the same pilot moves on to 2.
		{ID: 1, System: 1, At: 100, Reporter: "Scout A", Kind: KindSighting, Hostiles: 2,
			Analysis: Analysis{Pilots: []Pilot{hostile(9, "Bad Guy")}}},
		// System 2: two scouts, the second says it is a camp with a Sabre.
		{ID: 2, System: 2, At: 160, Reporter: "Scout B", Kind: KindSighting,
			Analysis: Analysis{Pilots: []Pilot{hostile(9, "Bad Guy")}}},
		{ID: 3, System: 2, At: 170, Reporter: "Scout C", Kind: KindCamp, Hostiles: 5,
			Analysis: Analysis{Pilots: []Pilot{hostile(10, "Other Guy")}, Ships: []Ship{{TypeID: 22456, Name: "Sabre", N: 1}}, Gate: 1}},
		// System 3: hostiles, then cleared.
		{ID: 4, System: 3, At: 100, Reporter: "Scout D", Kind: KindFleet, Hostiles: 20},
		{ID: 5, System: 3, At: 150, Reporter: "Scout D", Kind: KindClear, Clear: true},
		// Chatter counts for nothing.
		{ID: 6, System: 4, At: 150, Reporter: "Scout E", Kind: KindChatter},
	}
	s := Situations(reports, adj, 600)
	if len(s) != 3 {
		t.Fatalf("%d situations, want 3 (chatter is none): %+v", len(s), s)
	}
	camp := s[0]
	if camp.System != 2 || camp.Kind != KindCamp || camp.Reporters != 2 || camp.Count != 5 || len(camp.Pilots) != 2 || camp.Gate != 1 {
		t.Errorf("the camp first, both scouts and pilots taken together: %+v", camp)
	}
	if camp.From != 1 {
		t.Errorf("Bad Guy came from system 1: from %d", camp.From)
	}
	var cleared Situation
	for _, x := range s {
		if x.System == 3 {
			cleared = x
		}
	}
	if !cleared.Calm() || cleared.Kind != KindClear || len(cleared.Reports) != 1 {
		t.Errorf("a clear after a fleet leaves the system clear, its earlier reports left out: %+v", cleared)
	}
}
