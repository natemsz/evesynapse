package app

import "testing"

// TestFitBrowserModuleFilters: the shared fit browser's with/without
// module filters (ported from the public browser) keep only fits
// carrying the included module and none of the excluded one, and
// surface as removable active chips.
func TestFitBrowserModuleFilters(t *testing.T) {
	cards := []fitCard{
		{Name: "Brawler", Ship: "Rifter", moduleHay: "200mm autocannon ii\nsmall armor repairer ii"},
		{Name: "Kiter", Ship: "Slicer", moduleHay: "dual light pulse laser ii\nsmall ancillary armor repairer"},
		{Name: "Unfit", Ship: "Punisher"},
	}
	b := &fitBrowser{Path: "/doctrines/fits/", Include: "Autocannon", Exclude: "ancillary"}
	view := b.run(cards)
	if len(view.Cards) != 1 || view.Cards[0].Name != "Brawler" {
		t.Fatalf("module filters kept %+v, want only Brawler", view.Cards)
	}
	if len(view.Active) != 2 {
		t.Fatalf("active chips = %+v, want the include and exclude chips", view.Active)
	}
	if got := b.address(map[string]string{"include": ""}); got != "/doctrines/fits/?exclude=ancillary" {
		t.Fatalf("clearing the include filter gives %q, want the exclude kept", got)
	}
}
