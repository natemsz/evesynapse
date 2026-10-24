package app

import (
	"strings"
	"testing"
	"time"

	"evesynapse/internal/scan"
)

// The owner's Teonusude paste, as the scan parser reads it: a
// tower and a force field on grid, a station at 0 m, and two
// ships off grid named for their pilots. The sentence anchors on
// the station, counts the ships alone, and attributes them.
func TestDscanSentence(t *testing.T) {
	rows := []scan.DScanRow{
		{TypeID: 16214, Name: "Minmatar Control Tower", TypeName: "Minmatar Control Tower", DistKm: 4907},
		{TypeID: 601, Name: "BlackCatRus", TypeName: "Ibis", DistKm: 25000},
		{TypeID: 606, Name: "LegendMysticDragon", TypeName: "Velator", DistKm: 30000},
		{TypeID: 16103, Name: "Force Field", TypeName: "Force Field", DistKm: 4875},
		{TypeID: 10001, Name: "Teonusude VII - Moon 1 - Vherokior Tribe Treasury", TypeName: "Minmatar Military Station", DistKm: 0},
	}
	cats := map[int64]int64{16214: catStarbase, 601: catShip, 606: catShip, 16103: catStarbase, 10001: catStation}
	pilots := map[string]int64{"blackcatrus": 91000013, "legendmysticdragon": 91000014}
	pilotOf := func(name string) (int64, string, bool) {
		id, ok := pilots[strings.ToLower(name)]
		return id, name, ok
	}
	got, split := dscanSentence(&dscanReading{}, rows, cats, "Teonusude",
		func(int64) int { return bucketUnknown }, pilotOf, func(int64) bool { return false }, 0, 0)
	want := "At the Moon 1 station in Teonusude: 0 hostiles, 0 friendly, 0 neutral, 2 unclassified ships (Ibis, Velator), 0 on grid — BlackCatRus in the Ibis, LegendMysticDragon in the Velator."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if wantSplit := [4]int{0, 0, 0, 2}; split != wantSplit {
		t.Errorf("the split the bar shows: got %v, want %v", split, wantSplit)
	}
}

// The owner's local form: 27 in local, the paster among the
// friendlies, hostile name clusters read for alts, and the gap
// stated -- no scan attached, ships unknown.
func TestLocalSentence(t *testing.T) {
	names := []string{
		"Ari Zux", "Boro Zux", "Cato Zux", "Stevi Ka", "Stevinio Ka", "Stevo Ka", // hostiles
		"Paster One", "Ally Two", "Ally Three", "Ally Four", // friendlies, the paster first
	}
	for i := 1; i <= 17; i++ {
		names = append(names, "Neut "+string(rune('A'+i-1))+string(rune('a'+i%26)))
	}
	l := &localReading{}
	hostile := map[string]bool{}
	for _, n := range names[:6] {
		hostile[n] = true
	}
	for i, n := range names {
		p := localPilot{ID: int64(i + 1), Name: n, CorpID: 300}
		l.Pilots = append(l.Pilots, p)
	}
	standingOf := func(p localPilot) int {
		switch {
		case hostile[p.Name]:
			return bucketHostile
		case p.ID >= 7 && p.ID <= 10:
			return bucketFriendly
		default:
			return bucketNeutral
		}
	}
	got, _ := localSentence(l, 55, 7, time.Now(), standingOf, 0, 0)
	want := "27 in local: 6 hostiles, 4 friendly (incl. you), 17 neutral; clusters: Zux ×3, Stevi/Stevinio/Stevo Ka (6 hostile names ≈ 4 pilots) — no scan attached, ships unknown."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// A neutral whose stored kills put them in this system within
// the week is flagged in the sentence: standing understates them.
func TestLocalSentenceFlagsKiller(t *testing.T) {
	now := time.Now()
	l := &localReading{Pilots: []localPilot{
		{ID: 1, Name: "Bad One", CorpID: 201},
		{ID: 2, Name: "Quiet Rat", CorpID: 300, LastSystem: 55, LastKill: now.Add(-48 * time.Hour)},
	}}
	standingOf := func(p localPilot) int {
		if p.ID == 1 {
			return bucketHostile
		}
		return bucketNeutral
	}
	got, _ := localSentence(l, 55, 0, now, standingOf, 0, 0)
	want := "2 in local: 1 hostile, 0 friendly, 1 neutral — no scan attached, ships unknown; Quiet Rat killed here this week."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// A fleet composition names its pilots outright: attribution is
// plain, the counts by standing carry the hulls, and a member the
// site does not know is unclassified but still named.
func TestFleetSentence(t *testing.T) {
	f := &fleetReading{Members: []scan.FleetRow{
		{Name: "Ann Ace", Ship: "Vexor"},
		{Name: "Bob Bee", Ship: "Vexor"},
		{Name: "Cid Cee", Ship: "Sabre"},
	}}
	pilotOf := func(name string) (int64, string, bool) {
		switch name {
		case "Ann Ace":
			return 1, name, true
		case "Bob Bee":
			return 2, name, true
		}
		return 0, "", false
	}
	got, _ := fleetSentence(f, "Teonusude",
		func(int64) int { return bucketHostile }, pilotOf, func(int64) bool { return false })
	want := "In Teonusude: fleet of 3 — 2 hostiles (Vexor ×2), 0 friendly, 0 neutral, 1 unclassified pilot (Sabre) — Ann Ace in the Vexor, Bob Bee in the Vexor, Cid Cee in the Sabre."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
