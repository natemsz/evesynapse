package app

import (
	"database/sql"
	"math"
	"testing"
	"time"

	"evesynapse/internal/gamelog"
)

// TestFleetMergeCountsEveryShotOnce: two fleet pilots shoot each other
// and a shared enemy, and one of them holds two reads of the same
// seconds. Damage against the enemy is each pilot's own, a hit between
// them counts once as friendly fire, repairs count from the giver, and
// a kill both are on is one kill.
func TestFleetMergeCountsEveryShotOnce(t *testing.T) {
	const pilotA, pilotB = int64(1), int64(2)
	start := time.Unix(1_800_000_000, 0).UTC()
	step := gamelog.ClockStep(start.Unix())
	end := start.Add(9 * time.Second)
	reads := []*fleetLogRead{
		{ // A's log: party 0 is the enemy, party 1 is B.
			ID: 10, Character: pilotA, Start: start, End: end, Received: start.Add(time.Hour), Usable: true,
			Fleetmates: map[int64]int64{1: pilotB},
			Steps:      []gamelog.AlignedSlice{{At: step, Rows: [][7]int64{{0, 100, 40, 0, 0, 0, 0}, {1, 30, 20, 0, 0, 0, 0}}}},
		},
		{ // An older read of the same seconds of A's log.
			ID: 11, Character: pilotA, Start: start, End: end, Received: start.Add(time.Minute), Usable: true,
			Fleetmates: map[int64]int64{1: pilotB},
			Steps:      []gamelog.AlignedSlice{{At: step, Rows: [][7]int64{{0, 100, 40, 0, 0, 0, 0}, {1, 30, 20, 0, 0, 0, 0}}}},
		},
		{ // B's log: party 0 is A, party 1 is the enemy.
			ID: 20, Character: pilotB, Start: start, End: end, Received: start.Add(time.Hour), Usable: true,
			Fleetmates: map[int64]int64{0: pilotA},
			Steps:      []gamelog.AlignedSlice{{At: step, Rows: [][7]int64{{0, 20, 30, 15, 0, 0, 0}, {1, 50, 0, 0, 0, 0, 0}}}},
		},
	}
	on := map[int64]fleetPresence{
		pilotA: {From: start.Add(-time.Minute), To: end.Add(time.Minute)},
		pilotB: {From: start.Add(-time.Minute), To: end.Add(time.Minute)},
	}
	got := mergeFleetLogs(reads, countedByRead(reads), on)
	if got.Out != 150 || got.In != 40 || got.Friendly != 50 || got.Reps != 15 {
		t.Fatalf("out %d in %d friendly %d reps %d, want 150, 40, 50, 15", got.Out, got.In, got.Friendly, got.Reps)
	}

	mail := &fleetMail{
		ID: 7, At: start, System: 30000142, Victim: 999, Ship: 587, Damage: 150,
		Value: sql.NullFloat64{Float64: 100_000_000, Valid: true}, Counts: true,
		Attackers: []fleetMailAttacker{{Character: pilotA, Damage: 100}, {Character: pilotB, Damage: 50}},
	}
	clusters := clusterFleetBattles([]*fleetMail{mail}, nil, on)
	if len(clusters) != 1 || len(clusters[0].Mails) != 1 {
		t.Fatalf("one mail made %d battles", len(clusters))
	}
	battle, ok := composeFleetBattle(1, 0, clusters[0], nil, on, map[int64]int64{}, start)
	if !ok || battle.Battle.Kills != 1 || battle.Battle.IskDestroyed != 100_000_000 {
		t.Fatalf("a kill two pilots are on: %+v", battle.Battle)
	}
	scores := fleetMailScores(mail)
	total := 0.0
	for _, s := range scores {
		total += s.ValuePoints
	}
	if len(scores) != 2 || math.Abs(total-10) > 1e-9 {
		t.Fatalf("value points %v across %d pilots, want 10 across 2", total, len(scores))
	}
}
