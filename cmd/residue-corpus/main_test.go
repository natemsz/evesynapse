package main

import (
	"testing"

	"evesynapse/internal/intelanalyze"
)

func testIndex() {
	systemNames = map[int64]string{30002385: "Teonusude", 30002387: "Bosena"}
	adjacency = map[int64]map[int64]bool{
		30002385: {30002387: true},
		30002387: {30002385: true},
	}
}

// The label logic decides what the corpus teaches; a silent
// mislabel poisons training, so the decisive cases are pinned.
func TestClassify(t *testing.T) {
	testIndex()

	t.Run("embedded id and name system claims confirm", func(t *testing.T) {
		v := reportView{ReportID: 43, SystemID: 30002385, Text: "to Bosena gate is", Gate: 30002387}
		target, rejected, _, derived := classify([]string{"to", "is"}, []claim{
			{Span: "to", Kind: "system", Name: "30002385 Teonusude"},
			{Span: "is", Kind: "system", Name: "30002387 Bosena"},
		}, v)
		if len(rejected) != 0 || len(target) != 2 || derived {
			t.Fatalf("target=%v rejected=%v derived=%v", target, rejected, derived)
		}
		if target[1].ID != 30002387 || target[1].Name != "Bosena" {
			t.Fatalf("gate claim not normalized: %+v", target[1])
		}
	})

	t.Run("a count that only matches the standing split does not verify", func(t *testing.T) {
		v := reportView{ReportID: 24, SystemID: 30002385, Analysis: intelanalyze.Analysis{
			Counts: &intelanalyze.StandingCounts{Neutral: 7},
		}}
		target, rejected, _, _ := classify([]string{"tb"}, []claim{
			{Span: "tb", Kind: "count", N: 7},
		}, v)
		if len(target) != 0 || len(rejected) != 1 || rejected[0].Reason != "count_not_in_final_reading" {
			t.Fatalf("target=%v rejected=%v", target, rejected)
		}
	})

	t.Run("hand-verified gate reading replaces the stale pilot", func(t *testing.T) {
		v := reportView{
			ReportID: 42, SystemID: 30002385, Text: "to Bosen gate is", Gate: 30002387,
			DenyPilots: map[int64]bool{2113644992: true},
			Analysis: intelanalyze.Analysis{
				Pilots: []intelanalyze.Pilot{{ID: 2113644992, Name: "Bosen"}},
			},
		}
		target, rejected, spans, derived := classify([]string{"to", "is"}, []claim{
			{Span: "to", Kind: "pilot", ID: 2113644992, Name: "Bosen"},
			{Span: "to", Kind: "direction", Name: "to"},
		}, v)
		if !derived || len(target) != 1 || target[0].Kind != "system" || target[0].ID != 30002387 {
			t.Fatalf("target=%v derived=%v", target, derived)
		}
		if target[0].Span != v.Text || len(spans) != 3 {
			t.Fatalf("gate claim not on the report line span: target=%v spans=%v", target, spans)
		}
		if len(rejected) != 2 || rejected[0].Reason != "invented_or_unresolved_id" {
			t.Fatalf("rejected=%v", rejected)
		}
	})

	t.Run("name-matched ship confirms, unresolved system rejects", func(t *testing.T) {
		v := reportView{ReportID: 24, SystemID: 30002385, Analysis: intelanalyze.Analysis{
			Ships: []intelanalyze.Ship{{TypeID: 35825, Name: "Raitaru"}},
		}}
		target, rejected, _, _ := classify([]string{"km35825", "Aeddin"}, []claim{
			{Span: "km35825", Kind: "ship", Name: "Raitaru"},
			{Span: "Aeddin", Kind: "system", Name: "Aeddin"},
		}, v)
		if len(target) != 1 || target[0].ID != 35825 {
			t.Fatalf("target=%v", target)
		}
		if len(rejected) != 1 || rejected[0].Reason != "unconfirmed_name" {
			t.Fatalf("rejected=%v", rejected)
		}
	})
}
