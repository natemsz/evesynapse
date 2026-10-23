package app

import (
	"math"
	"testing"
	"time"
)

// TestBuildThreat: kills decay by half every 45 minutes and stop
// counting after three hours; a camp is one group on most of 4+
// public kills spread over the last hour on passers-by, named by
// alliance first -- never a burst on one fleet, never from private
// kills.
func TestBuildThreat(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	kills := []threatKill{
		// Jita: one kill now, one 45 minutes ago, one past the window.
		{ID: 1, System: 30000142, Time: ago(0), Public: true},
		{ID: 2, System: 30000142, Time: ago(45), Public: true},
		{ID: 3, System: 30000142, Time: ago(200), Public: true},
		// Rancer: a camp. Five public kills over half an hour, passers-by
		// from different corps, the same alliance on four of them.
		{ID: 10, System: 30003504, Time: ago(5), Public: true, VictimCorp: 1},
		{ID: 11, System: 30003504, Time: ago(15), Public: true, VictimCorp: 2},
		{ID: 12, System: 30003504, Time: ago(25), Public: true, VictimCorp: 3},
		{ID: 13, System: 30003504, Time: ago(35), Public: true, VictimCorp: 4},
		{ID: 14, System: 30003504, Time: ago(40), Public: true, VictimCorp: 5},
		// Tama: a fight, not a camp. Five kills inside three minutes,
		// one fleet dying to one alliance.
		{ID: 30, System: 30002813, Time: ago(10), Public: true, VictimCorp: 7},
		{ID: 31, System: 30002813, Time: ago(11), Public: true, VictimCorp: 7},
		{ID: 32, System: 30002813, Time: ago(11), Public: true, VictimCorp: 7},
		{ID: 33, System: 30002813, Time: ago(12), Public: true, VictimCorp: 7},
		{ID: 34, System: 30002813, Time: ago(13), Public: true, VictimCorp: 7},
		// Amamake: three kills by one group, but two of them private:
		// it counts toward the score and names no camp.
		{ID: 20, System: 30002537, Time: ago(5), Public: true},
		{ID: 21, System: 30002537, Time: ago(6), Public: false},
		{ID: 22, System: 30002537, Time: ago(7), Public: false},
	}
	atk := []threatAttacker{
		{Kill: 10, Character: 1, Corp: 98000001, Alliance: 99000001},
		{Kill: 11, Character: 2, Corp: 98000001, Alliance: 99000001},
		{Kill: 12, Character: 1, Corp: 98000001, Alliance: 99000001},
		{Kill: 13, Character: 9, Corp: 1000125}, // an NPC corp: a lone pilot's group
		{Kill: 14, Character: 3, Corp: 98000002, Alliance: 99000001},
		{Kill: 20, Character: 5, Corp: 98000005},
	}
	for id := int64(30); id <= 34; id++ {
		atk = append(atk, threatAttacker{Kill: id, Character: 7, Corp: 98000009, Alliance: 99000009})
	}
	named := map[string]string{}
	th := buildThreat(now, kills, atk, func(kind string, id int64) string {
		named[kind] = "name"
		return "The Campers"
	})
	score := map[int64]int{}
	hour := map[int64]int{}
	for i, id := range th.ID {
		score[id] = th.Score[i]
		hour[id] = th.Hour[i]
	}
	if s := score[30000142]; math.Abs(float64(s)-150) > 1 {
		t.Errorf("Jita: a fresh kill (1) and one at the half-life (0.5) make 1.5, got %d/100", s)
	}
	if hour[30000142] != 2 {
		t.Errorf("Jita: two kills in the last hour, got %d", hour[30000142])
	}
	// Three kills 5-7 minutes old: about 2.74. More than two kills'
	// worth means the two private ones counted.
	if score[30002537] < 250 {
		t.Errorf("private kills still count toward the score: %d", score[30002537])
	}
	if len(th.Camps) != 1 {
		t.Fatalf("camps = %+v, want Rancer alone (Tama is a fight, Amamake private)", th.Camps)
	}
	c := th.Camps[0]
	if c.System != 30003504 || c.Kind != "alliance" || c.GroupID != 99000001 || c.Kills != 4 || c.Group != "The Campers" {
		t.Errorf("camp: %+v", c)
	}
	if c.Since != ago(40).Unix() {
		t.Errorf("a camp runs from its window's first kill: %d", c.Since)
	}
}

// TestBuildThreatStructureKills: structure and deployable kills are
// heat, never camp evidence. The acceptance shape: two campers
// farming sixteen tractor units at one gate over half an hour --
// every other camp signal fires (count, span, victim corps, one
// alliance on all of them) and it still names no camp, because none
// of the sixteen victims was a ship. A mixed system whose ship kills
// alone are under the threshold names no camp either, and a genuine
// camp's kill count ignores its structure kills.
func TestBuildThreatStructureKills(t *testing.T) {
	for cat, want := range map[int64]bool{22: true, 23: true, 65: true, 6: false, 3: false, 18: false, 0: false} {
		if got := structureCategory(cat); got != want {
			t.Errorf("structureCategory(%d) = %v, want %v", cat, got, want)
		}
	}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ago := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	const (
		farm  = int64(30009991) // structures only
		mixed = int64(30009992) // ships under threshold + structures
		real  = int64(30009993) // a genuine camp, plus structure noise
	)
	var kills []threatKill
	var atk []threatAttacker
	id := int64(0)
	add := func(sys int64, minutes int, victimCorp int64, structure bool, attackers ...int64) int64 {
		id++
		kills = append(kills, threatKill{ID: id, System: sys, Time: ago(minutes), Public: true, VictimCorp: victimCorp, Structure: structure})
		for _, a := range attackers {
			atk = append(atk, threatAttacker{Kill: id, Character: a, Corp: 98000001, Alliance: 99000001})
		}
		return id
	}
	// The farm: sixteen structure kills over thirty minutes, the
	// victims from five different corporations, two pilots of one
	// alliance alternating on all of them.
	for i := 0; i < 16; i++ {
		pilot := int64(1)
		if i%2 == 1 {
			pilot = 2
		}
		add(farm, 2+2*i, int64(1+i%5), true, pilot)
	}
	// Mixed: three ship kills and thirteen structure kills, one
	// alliance on everything, spread over the same half hour.
	for i, m := range []int{5, 20, 35} {
		add(mixed, m, int64(1+i), false, 1)
	}
	for i := 0; i < 13; i++ {
		add(mixed, 3+2*i, int64(1+i%5), true, 1)
	}
	// A genuine camp: five ship kills on passers-by over forty
	// minutes, the alliance on four -- plus ten structure kills by
	// the same alliance, which must not inflate its count.
	for i, m := range []int{5, 15, 25, 35, 40} {
		if i == 3 {
			odd := add(real, m, int64(11+i), false)
			atk = append(atk, threatAttacker{Kill: odd, Character: 9, Corp: 1000125}) // an NPC corp: not the alliance
			continue
		}
		add(real, m, int64(11+i), false, 1)
	}
	for i := 0; i < 10; i++ {
		add(real, 6+3*i, int64(1+i%5), true, 1)
	}
	th := buildThreat(now, kills, atk, func(kind string, gid int64) string { return "The Campers" })
	hour := map[int64]int{}
	for i, sys := range th.ID {
		hour[sys] = th.Hour[i]
	}
	if hour[farm] != 16 {
		t.Errorf("the farm's kills still count as heat: hour = %d, want 16", hour[farm])
	}
	if len(th.Camps) != 1 {
		t.Fatalf("camps = %+v, want the genuine camp alone", th.Camps)
	}
	c := th.Camps[0]
	if c.System != real || c.Kills != 4 {
		t.Errorf("camp = %+v, want the genuine camp with its 4 ship kills (structure kills uncounted)", c)
	}
}
