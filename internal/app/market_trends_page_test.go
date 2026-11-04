package app

import "testing"

// The ranking is cached and shared between readers, so opening an
// item lands on a copy: the cached rows never carry one reader's
// selection into another's page.
func TestMarketTrendsSelectionCopies(t *testing.T) {
	cached := &marketTrendsView{Rows: []marketTrendRow{{TypeID: 34, Rank: 1}, {TypeID: 35, Rank: 2}}}
	got := cached.withSelection("35")
	if got.Depth != 2 || got.Insp == nil || got.Insp.TypeID != 35 || !got.Rows[1].On {
		t.Fatalf("selection = depth %d, insp %+v, want type 35 open at depth 2", got.Depth, got.Insp)
	}
	if cached.Rows[1].On || cached.Insp != nil || cached.Depth != 0 {
		t.Fatalf("the cached view took the selection: %+v", cached)
	}
	if none := cached.withSelection("999"); none.Depth != 1 || none.Insp != nil {
		t.Fatalf("an unranked type opened the inspector: %+v", none)
	}
	if bad := cached.withSelection("x"); bad.Depth != 1 || bad.Insp != nil {
		t.Fatalf("a malformed type opened the inspector: %+v", bad)
	}
}
