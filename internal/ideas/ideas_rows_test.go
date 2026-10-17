package ideas

import (
	"reflect"
	"strings"
	"testing"
)

// The golden fixture: one data set as a legacy answer (the model
// transcribed the figures), as a new answer (the model references
// rows and judges), and the rows as sent. Composing the new answer
// must render EXACTLY the legacy lines, page and brief forms alike.
var goldenRows = []Row{
	{N: 1, Item: "Tritanium", Station: "Jita IV - Moon 4 - Caldari Navy Assembly Plant", Buy: "4.00", Sell: "10.00", Spread: "150.0%", Sold: "20", Profit: "120.00"},
	{N: 2, Item: "Pyerite", Station: "Amarr VIII - Oris - Emperor Family Academy", Buy: "90.10", Sell: "100.40", Spread: "11.4%", Sold: "2,000", Profit: "20,600.00"},
}

const goldenLegacy = "### Station Flips\n" +
	"*Fast movers at the hub.*\n" +
	"* **Tritanium** · Jita IV - Moon 4 - Caldari Navy Assembly Plant · Typical buy 4.00 · Typical sell 10.00 · Spread 150.0% · 20 sold per day · Estimated daily profit 120.00 — deep volume at the hub\n" +
	"* **Pyerite** · Amarr VIII - Oris - Emperor Family Academy · Typical buy 90.10 · Typical sell 100.40 · Spread 11.4% · 2,000 sold per day · Estimated daily profit 20,600.00 — steady margin, calm route\n"

const goldenModern = "### Station Flips\n" +
	"*Fast movers at the hub.*\n" +
	"* **Tritanium** (#1) — deep volume at the hub\n" +
	"* **Pyerite** (#2) — steady margin, calm route\n"

func TestGoldenNewAnswerComposesToLegacyLines(t *testing.T) {
	legacy := Parse(goldenLegacy, nil)
	modern := Parse(goldenModern, goldenRows)
	if len(legacy) != 1 || len(modern) != 1 {
		t.Fatalf("groups: legacy %d, modern %d", len(legacy), len(modern))
	}
	if !reflect.DeepEqual(modern[0].Ideas, legacy[0].Ideas) {
		t.Errorf("composed ideas differ from the legacy lines:\nmodern %q\nlegacy %q", modern[0].Ideas, legacy[0].Ideas)
	}
	if !reflect.DeepEqual(modern[0].Briefs, legacy[0].Briefs) {
		t.Errorf("composed briefs differ from the legacy briefs:\nmodern %q\nlegacy %q", modern[0].Briefs, legacy[0].Briefs)
	}
	for _, idea := range modern[0].Ideas {
		if strings.Contains(string(idea), "(#") {
			t.Errorf("a row reference leaked into the rendered line: %q", idea)
		}
	}
}

// The displayed item name comes from the ROW, not the model's text:
// a transcription slip in the name cannot reach the page.
func TestParseTakesItemNameFromTheRow(t *testing.T) {
	groups := Parse("### Flips\n* **Trit** (#1) — deep volume\n", goldenRows)
	if len(groups) != 1 || len(groups[0].Ideas) != 1 {
		t.Fatalf("groups: %+v", groups)
	}
	got := string(groups[0].Ideas[0])
	if !strings.Contains(got, "<strong>Tritanium</strong>") || strings.Contains(got, "Trit<") {
		t.Errorf("the row's item name did not win: %q", got)
	}
}

// Each fallback keeps the line: the model's own item and reason,
// with no figure segment and no reference.
func TestParseReferenceFallbacks(t *testing.T) {
	for name, line := range map[string]string{
		"missing reference":     "* **Pyerite** — plain reason",
		"out of range":          "* **Pyerite** (#99) — far reason",
		"unparseable reference": "* **Pyerite** (#x) — odd reason",
	} {
		groups := Parse("### Flips\n"+line+"\n", goldenRows)
		if len(groups) != 1 || len(groups[0].Ideas) != 1 {
			t.Fatalf("%s: groups %+v", name, groups)
		}
		got := string(groups[0].Ideas[0])
		if strings.Contains(got, "Typical buy") || strings.Contains(got, "(#") {
			t.Errorf("%s: fallback line carries figures or a reference: %q", name, got)
		}
		if !strings.Contains(got, "<strong>Pyerite</strong>") || !strings.Contains(got, "reason") {
			t.Errorf("%s: fallback lost the item or the reason: %q", name, got)
		}
	}
}

// A stored answer with no row set (every answer written before rows
// were stored) parses verbatim, exactly as today.
func TestParseLegacyBodyWithoutRows(t *testing.T) {
	groups := Parse(goldenLegacy, nil)
	if len(groups) != 1 || len(groups[0].Ideas) != 2 {
		t.Fatalf("groups: %+v", groups)
	}
	if !strings.Contains(string(groups[0].Ideas[0]), "Typical buy 4.00") {
		t.Errorf("a legacy line did not render verbatim: %q", groups[0].Ideas[0])
	}
	if got := DecodeRows(""); got != nil {
		t.Errorf("an empty stored row set decoded to %v, want nil", got)
	}
	if got := DecodeRows("{not json"); got != nil {
		t.Errorf("a damaged stored row set decoded to %v, want nil", got)
	}
	raw, err := EncodeRows(goldenRows)
	if err != nil || !reflect.DeepEqual(DecodeRows(raw), goldenRows) {
		t.Errorf("the row set did not round-trip: %v (%q)", err, raw)
	}
}
