package buildplanner

import (
	"encoding/json"
	"strings"
	"testing"

	"evesynapse/internal/buildplan"
	db "evesynapse/internal/db/sqlc"
	"evesynapse/internal/esi"
)

// TestPlanMESummary: the ME wall compresses to one line — the
// all-default count, otherwise each non-default blueprint with
// the rest in one tail.
func TestPlanMESummary(t *testing.T) {
	if got := planMESummary(nil); got != "" {
		t.Errorf("empty = %q, want empty", got)
	}
	def := []planMEInput{
		{Label: "A", Value: 0, Source: "assumed"},
		{Label: "B", Value: 0, Source: "assumed"},
	}
	if got := planMESummary(def); got != "2 components at ME0 (assumed)" {
		t.Errorf("default = %q, want the count line", got)
	}
	solo := []planMEInput{{Label: "A", Value: 0, Source: "assumed"}}
	if got := planMESummary(solo); got != "1 component at ME0 (assumed)" {
		t.Errorf("solo = %q, want singular", got)
	}
	mixed := []planMEInput{
		{Label: "Fixture Component", Value: 8, Source: "owned"},
		{Label: "Fixture Widget", Value: 0, Source: "assumed"},
	}
	if got := planMESummary(mixed); got != "Fixture Component at ME8 (owned), 1 at ME0 (assumed)" {
		t.Errorf("mixed = %q, want non-default listed with tail", got)
	}
}

// TestDefaultJudgeParam: fresh plans scope to the acting
// character, falling back to the first character when the
// session names none of the account's own, and to the All V
// stand-in when there are no characters (an anonymous visitor).
func TestDefaultJudgeParam(t *testing.T) {
	chars := []db.Character{{CharacterID: 1}, {CharacterID: 2}}
	if got := defaultJudgeParam(chars, 2); got != "char:2" {
		t.Errorf("acting = %q, want char:2", got)
	}
	if got := defaultJudgeParam(chars, 9); got != "char:1" {
		t.Errorf("stale session = %q, want char:1", got)
	}
	if got := defaultJudgeParam(nil, 9); got != judgeAllV {
		t.Errorf("no characters = %q, want the All V stand-in", got)
	}
}

// An empty preset (the mask buys the finished product, or stock
// covers everything) must marshal its lists as [], never null —
// the client dereferences both on first load, and a null shopping
// list is what blanked capital plans until a preset was clicked.
func TestPlannerEmptyPresetMarshalsLists(t *testing.T) {
	r := &planRollup{preset: planPresetOptimized, shop: make(map[int64]int64)}
	r.tot.Complete = true
	r.tot.FeesKnown = true
	root := &buildplan.Node{TypeID: 999, ProducedQty: 1}
	planFinishPreset(r, root, map[int64]esi.MarketPrice{}, 1,
		func(int64) string { return "X" },
		func(int64) (float64, bool) { return 0, false })
	raw, err := json.Marshal(r.tot)
	if err != nil {
		t.Fatalf("marshal totals: %v", err)
	}
	for _, want := range []string{`"shopping":[]`, `"jobs":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty preset marshaled %s missing %s (null lists break first load)", raw, want)
		}
	}
}
