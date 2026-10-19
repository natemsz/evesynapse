package appraise

import (
	"strings"
	"testing"

	db "evesynapse/internal/db/sqlc"
)

func firstCand(t *testing.T, line string) (pasteLine, nameCandidate) {
	t.Helper()
	pl, ok := parseListLine(line)
	if !ok {
		t.Fatalf("parseListLine(%q): not ok", line)
	}
	if len(pl.Candidates) == 0 {
		t.Fatalf("parseListLine(%q): no candidates", line)
	}
	return pl, pl.Candidates[0]
}

func TestParsePlainQuantities(t *testing.T) {
	cases := []struct {
		line string
		name string
		qty  int64
	}{
		{"Tritanium x 1,000,000", "Tritanium", 1000000},
		{"Tritanium x1000", "Tritanium", 1000},
		{"1,000,000x Tritanium", "Tritanium", 1000000},
		{"500 x Pyerite", "Pyerite", 500},
		{"Pyerite, 250", "Pyerite", 250},
		{"Tritanium\t1,000\tMineral\tMaterial", "Tritanium", 1000},
		{"Mexallon\t250", "Mexallon", 250},
		{"Rifter", "Rifter", 1},
	}
	for _, c := range cases {
		_, cand := firstCand(t, c.line)
		if cand.Name != c.name || cand.Qty != c.qty {
			t.Errorf("parseListLine(%q) first candidate = {%q, %d}, want {%q, %d}",
				c.line, cand.Name, cand.Qty, c.name, c.qty)
		}
	}
}

func TestParseTrailingQuantityIsASecondCandidate(t *testing.T) {
	// No explicit quantity: the name as typed wins precedence,
	// the trailing-number reading follows it.
	pl, first := firstCand(t, "Tritanium 1000000")
	if first.Name != "Tritanium 1000000" || first.Qty != 1 {
		t.Errorf("first candidate = {%q, %d}, want the name as typed at qty 1", first.Name, first.Qty)
	}
	if len(pl.Candidates) != 2 || pl.Candidates[1].Name != "Tritanium" || pl.Candidates[1].Qty != 1000000 {
		t.Errorf("candidates = %+v, want a {Tritanium, 1000000} second reading", pl.Candidates)
	}

	// Thousand-group spacing reads as one number.
	pl, _ = firstCand(t, "Tritanium 1 000 000")
	if len(pl.Candidates) != 2 || pl.Candidates[1].Qty != 1000000 {
		t.Errorf("grouped candidates = %+v, want qty 1000000", pl.Candidates)
	}

	// Ambiguous trailing numbers carry both readings,
	// grouped first: "Widget 5 250" can be 5,250 Widgets or
	// 250 of the type "Widget 5"; resolution order decides.
	pl, _ = firstCand(t, "Widget 5 250")
	if len(pl.Candidates) != 3 || pl.Candidates[1].Name != "Widget" || pl.Candidates[1].Qty != 5250 ||
		pl.Candidates[2].Name != "Widget 5" || pl.Candidates[2].Qty != 250 {
		t.Errorf("ambiguous candidates = %+v, want {Widget, 5250} then {Widget 5, 250}", pl.Candidates)
	}
}

func TestParseNumberNamedTypeKeepsItsName(t *testing.T) {
	// "Cap Booster 3200" is a type name, not a quantity: the
	// full name is the first candidate, and an explicit
	// quantity leaves it untouched.
	pl, first := firstCand(t, "Cap Booster 3200")
	if first.Name != "Cap Booster 3200" || first.Qty != 1 {
		t.Errorf("first candidate = {%q, %d}, want the full type name", first.Name, first.Qty)
	}
	pl, first = firstCand(t, "Cap Booster 3200 x 5")
	if len(pl.Candidates) != 1 || first.Name != "Cap Booster 3200" || first.Qty != 5 {
		t.Errorf("explicit candidates = %+v, want one {Cap Booster 3200, 5}", pl.Candidates)
	}
}

func TestParseBlueprintAnnotations(t *testing.T) {
	pl, cand := firstCand(t, "Rifter Blueprint (BPC, ME 10, TE 20, 5 runs)")
	if cand.Name != "Rifter Blueprint" {
		t.Errorf("name = %q, want the annotation stripped", cand.Name)
	}
	if !pl.HasFacts || !pl.Copy || !pl.HasME || pl.ME != 10 || !pl.HasTE || pl.TE != 20 || !pl.HasRuns || pl.Runs != 5 {
		t.Errorf("facts = %+v, want BPC ME10 TE20 5 runs", pl)
	}
	pl, cand = firstCand(t, "Rifter Blueprint [BPO ME9 TE18]")
	if cand.Name != "Rifter Blueprint" || !pl.HasFacts || pl.Copy || pl.ME != 9 || pl.TE != 18 || pl.HasRuns {
		t.Errorf("bracket facts = %+v (name %q), want BPO ME9 TE18", pl, cand.Name)
	}
	// A parenthesised group with no blueprint vocabulary is
	// part of the name, not an annotation.
	_, cand = firstCand(t, "Widget (Prototype)")
	if cand.Name != "Widget (Prototype)" {
		t.Errorf("name = %q, want the parenthesis kept", cand.Name)
	}
}

func TestParseAbyssalCandidates(t *testing.T) {
	pl, first := firstCand(t, "Abyssal Damage Control II")
	if first.Name != "Abyssal Damage Control II" || first.AbyssalBase {
		t.Errorf("first candidate = %+v, want the name as typed", first)
	}
	last := pl.Candidates[len(pl.Candidates)-1]
	if last.Name != "Damage Control II" || !last.AbyssalBase {
		t.Errorf("last candidate = %+v, want the stripped base flagged abyssal", last)
	}
}

func TestParseShowinfoItemLink(t *testing.T) {
	pl, first := firstCand(t, "<url=showinfo:60001//900001>Abyssal Damage Control II</url>")
	if pl.LinkTypeID != 60001 || pl.LinkItemID != 900001 {
		t.Errorf("link ids = %d//%d, want 60001//900001", pl.LinkTypeID, pl.LinkItemID)
	}
	// The mutation-stripped base reading leads: it is the
	// fallback price for when the item's rolls are not on
	// record; the name as linked follows it.
	if first.Name != "Damage Control II" || first.Qty != 1 || !first.AbyssalBase {
		t.Errorf("first candidate = %+v, want the stripped base at qty 1", first)
	}
	last := pl.Candidates[len(pl.Candidates)-1]
	if last.Name != "Abyssal Damage Control II" || last.Qty != 1 || last.AbyssalBase {
		t.Errorf("last candidate = %+v, want the name as linked at qty 1", last)
	}

	// Quantity text around the link does not multiply a
	// unique item.
	pl, first = firstCand(t, "2 x <url=showinfo:60001//900001>Abyssal Damage Control II</url>")
	if pl.LinkItemID != 900001 || first.Qty != 1 {
		t.Errorf("quantity-wrapped link first candidate = %+v (item %d), want qty 1 for item 900001", first, pl.LinkItemID)
	}
}

func TestParseShowinfoTypeLink(t *testing.T) {
	// A type link names only the type: it unwraps to its
	// display text and the ordinary readings apply.
	pl, first := firstCand(t, "<url=showinfo:34>Tritanium</url>")
	if pl.LinkItemID != 0 || pl.LinkTypeID != 0 || first.Name != "Tritanium" || first.Qty != 1 {
		t.Errorf("type link = %+v (ids %d//%d), want a plain Tritanium line", first, pl.LinkTypeID, pl.LinkItemID)
	}
	_, first = firstCand(t, "500 x <url=showinfo:34>Tritanium</url>")
	if first.Name != "Tritanium" || first.Qty != 500 {
		t.Errorf("quantified type link = %+v, want 500 Tritanium", first)
	}
}

func TestParseBadAndTruncation(t *testing.T) {
	if _, ok := parseListLine("(BPC, ME 10)"); ok {
		t.Error("an annotation with no item name must not parse")
	}
	res := parsePaste(strings.Repeat("Tritanium x 1\n", maxPasteLines+5))
	if !res.Truncated {
		t.Error("over-cap paste must flag truncation")
	}
	if len(res.Lines) != maxPasteLines {
		t.Errorf("lines = %d, want the %d cap", len(res.Lines), maxPasteLines)
	}
	res = parsePaste("\n\n  \nTritanium x 2\n# a comment\n")
	if len(res.Lines) != 1 || len(res.Bad) != 0 {
		t.Errorf("blank/comment handling = %+v, want one clean line", res)
	}
}

func TestAggregateAndNotes(t *testing.T) {
	agg := aggregateLines([]resolvedLine{
		{TypeID: 34, Name: "Tritanium", Qty: 3},
		{TypeID: 34, Name: "Tritanium", Qty: 4},
		{TypeID: 7001, Name: "Rifter Blueprint", Qty: 1, Facts: pasteLine{HasFacts: true, Copy: true, HasME: true, ME: 10, HasTE: true, TE: 20, HasRuns: true, Runs: 5}},
		{TypeID: 7001, Name: "Rifter Blueprint", Qty: 2},
	})
	if len(agg) != 3 {
		t.Fatalf("agg = %+v, want 3 lines (same-type stacks merge, differing blueprint facts do not)", agg)
	}
	if agg[0].Qty != 7 {
		t.Errorf("merged qty = %d, want 7", agg[0].Qty)
	}
	if agg[1].Note != "BPC · ME 10 · TE 20 · 5 runs" {
		t.Errorf("bpc note = %q", agg[1].Note)
	}
	if agg[2].Note != "Blueprint — priced as an original" {
		t.Errorf("bare blueprint note = %q", agg[2].Note)
	}
	note := lineNote(resolvedLine{TypeID: 30001, Name: "Damage Control II", AbyssalBase: true})
	if !strings.Contains(note, "Base item price") || !strings.Contains(note, "without its link") {
		t.Errorf("abyssal base note = %q", note)
	}
	if strings.Contains(note, "Contract Search") {
		t.Errorf("abyssal base note = %q, must not imply mutated items cannot be appraised here", note)
	}
}

func TestAggregateKeepsUniqueItemsApart(t *testing.T) {
	// Two different mutated items of one type, carrying the
	// same estimate note, stay two lines; the same item
	// pasted twice still merges with itself.
	mk := func(itemID int64) resolvedLine {
		return resolvedLine{TypeID: 60001, Name: "Abyssal Damage Control II", Qty: 1, ItemID: itemID,
			Abyssal: &db.AbyssalItem{ItemID: itemID, EstBasis: "corpus", EstScope: "module", EstComps: 8}}
	}
	agg := aggregateLines([]resolvedLine{mk(900001), mk(900002), mk(900001)})
	if len(agg) != 2 {
		t.Fatalf("agg = %+v, want two lines: one per unique item", agg)
	}
	if agg[0].ItemID != 900001 || agg[0].Qty != 2 {
		t.Errorf("first line = %+v, want item 900001 (its duplicate merges with itself)", agg[0])
	}
	if agg[1].ItemID != 900002 || agg[1].Qty != 1 {
		t.Errorf("second line = %+v, want item 900002 at qty 1", agg[1])
	}
	if agg[0].Note != "from 8 listings of this module" {
		t.Errorf("abyssal estimate note = %q", agg[0].Note)
	}
	note := lineNote(resolvedLine{TypeID: 30001, Name: "Damage Control II", LinkMiss: true})
	if note != "Base item price — this item's rolls are not in our records" {
		t.Errorf("link miss note = %q", note)
	}
}
