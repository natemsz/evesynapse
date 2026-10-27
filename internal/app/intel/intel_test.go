package intel

import (
	"testing"

	"evesynapse/internal/esi"
)

// TestContestedPercent: victory points over the flip threshold, capped
// at 100, and -1 for a system with no live contest.
func TestContestedPercent(t *testing.T) {
	cases := []struct {
		name string
		sys  esi.FWSystem
		want int
	}{
		{"contested", esi.FWSystem{Contested: "contested", VictoryPoints: 3000, VictoryPointsThreshold: 10000}, 30},
		{"vulnerable caps at 100", esi.FWSystem{Contested: "vulnerable", VictoryPoints: 12000, VictoryPointsThreshold: 10000}, 100},
		{"uncontested has none", esi.FWSystem{Contested: "uncontested", VictoryPoints: 500, VictoryPointsThreshold: 10000}, -1},
		{"no threshold", esi.FWSystem{Contested: "contested", VictoryPoints: 500}, -1},
	}
	for _, c := range cases {
		if got := contestedPercent(c.sys); got != c.want {
			t.Errorf("%s: contestedPercent = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestWarInvolves: a war is yours when its aggressor, defender or a
// defending ally is one of your corporations; nobody's corporations
// match nothing.
func TestWarInvolves(t *testing.T) {
	war := esi.War{
		Aggressor: esi.WarParty{CorporationID: 1},
		Defender:  esi.WarParty{CorporationID: 2},
		Allies:    []esi.WarAlly{{CorporationID: 3}},
	}
	for id, want := range map[int64]bool{1: true, 2: true, 3: true, 4: false} {
		if got := warInvolves(war, map[int64]bool{id: true}); got != want {
			t.Errorf("corporation %d: warInvolves = %v, want %v", id, got, want)
		}
	}
	if warInvolves(war, nil) {
		t.Error("a user with no corporations has a war flagged as theirs")
	}
}

func TestFactionDisplay(t *testing.T) {
	names := map[int64]string{500001: "Caldari State"}
	if got := factionDisplay(names, 500001); got != "Caldari State" {
		t.Errorf("known faction: %q", got)
	}
	if got := factionDisplay(names, 500002); got != "Faction #500002" {
		t.Errorf("unknown faction: %q", got)
	}
	if got := warPartyRecord(esi.WarParty{ShipsKilled: 12, ISKDestroyed: 1234567.89}); got != "12 ships · 1,234,567.89 ISK destroyed" {
		t.Errorf("warPartyRecord: %q", got)
	}
}

// TestModuleDeclaration: the module declares all five things (empty
// lists written out), needs no scope, and registers its three pages;
// its menu also leads with the core killboard, scans and channels, then the workbench (eight entries).
func TestModuleDeclaration(t *testing.T) {
	m := Module
	if m.ID != "intel" || m.Title == "" || m.Requires == nil || m.Scopes == nil || m.EnhanceScopes == nil {
		t.Fatalf("incomplete declaration: %+v", m)
	}
	if len(m.Scopes) != 0 || len(m.EnhanceScopes) != 0 {
		t.Errorf("intel is public data and asks for no scope: %+v %+v", m.Scopes, m.EnhanceScopes)
	}
	if len(m.Pages) != 3 || len(m.Sitemap) != 3 || len(m.Nav) != 1 || len(m.Nav[0].Entries) != 8 {
		t.Errorf("pages %d, sitemap %d, nav %+v; want 3 pages, 3 sitemap lines, one section of 8 entries", len(m.Pages), len(m.Sitemap), m.Nav)
	}
	if m.Nav[0].Entries[0].Href != "/intel/kills/" {
		t.Errorf("the killboard leads the section: %+v", m.Nav[0].Entries[0])
	}
}
