package gamelog

import (
	"strings"
	"testing"
	"time"
)

// A game log read end to end: the grammar, the gap that ends a
// fight, the rat-only fight that is never kept, and the tallies.
func TestCombatLogToFight(t *testing.T) {
	log := strings.Join([]string{
		"------------------------------------------------------------",
		"  Gamelog",
		"  Listener: Iwaira Itovuod",
		"  Session Started: 2026.11.04 12:00:00",
		"------------------------------------------------------------",
		// Rats only: not a battle.
		"[ 2026.11.04 12:00:01 ] (combat) <color=0xff00ffff><b>300</b> <color=0x77ffffff><font size=10>to</font> <b><color=0xffffffff>Guristas Destroyer</b><font size=10><color=0x77ffffff> - Heavy Pulse Laser II - Hits",
		// Two minutes later, a player.
		"[ 2026.11.04 12:03:00 ] (notify) Warp scramble attempt from <b>Bad Guy</b>[EVIL](Vexor) to <b>you!</b>",
		"[ 2026.11.04 12:03:01 ] (combat) <color=0xffcc0000><b>120</b> <color=0x77ffffff><font size=10>from</font> <b><color=0xffffffff>Bad Guy</b>[EVIL](Vexor)<font size=10><color=0x77ffffff> - Hammerhead II - Smashes",
		"[ 2026.11.04 12:03:01 ] (combat) <color=0xff00ffff><b>523</b> <color=0x77ffffff><font size=10>to</font> <b><color=0xffffffff>Bad Guy</b>[EVIL](Vexor)<font size=10><color=0x77ffffff> - Heavy Pulse Laser II - Penetrates",
		"[ 2026.11.04 12:03:02 ] (combat) Your group of Heavy Pulse Laser II misses Bad Guy[EVIL](Vexor) completely - Heavy Pulse Laser II",
		// Repairs, energy warfare and grid tackle name ships by hull.
		"[ 2026.11.04 12:03:03 ] (combat) <color=0xffccff66><b>352</b><color=0x77ffffff><font size=10> remote shield boosted by </font><b><color=0xffffffff><font size=12><color=0xFFFFB900> <u><b>Loki</b></u></color></font> </b><color=0x77ffffff><font size=10> - Pithum B-Type Medium Remote Shield Booster</font>",
		"[ 2026.11.04 12:03:04 ] (combat) <color=0xffe57f7f><b>140 GJ</b><color=0x77ffffff><font size=10> energy neutralized </font><b><color=0xffffffff><font size=12><color=0xFFFFB900> <u><b>Vexor</b></u></color></font> </b><color=0x77ffffff><font size=10> - Medium Energy Neutralizer II</font>",
		"[ 2026.11.04 12:03:04 ] (combat) <color=0xffffffff><b>Warp disruption attempt</b> <color=0x77ffffff><font size=10>from</font> <color=0xffffffff><b><font size=12><color=0xFFFFB900> <u><b>Crow</b></u></color></font> </b> <color=0x77ffffff><font size=10>to <b><color=0xffffffff></font><font size=12><color=0xFFFFB900> <u><b>Cenotaph</b></u></color></font> ",
		"[ 2026.11.04 12:03:04 ] (combat) <color=0xff7fffff><b>+0 GJ</b><color=0x77ffffff><font size=10> energy drained from </font><b><color=0xffffffff><font size=12><color=0xFFFFB900> <u><b>Vexor</b></u></color></font> </b><color=0x77ffffff><font size=10> - Medium Energy Nosferatu II</font>",
		"[ 2026.11.04 12:03:04 ] (combat) <color=0xff00ffff><b>40</b> <color=0x77ffffff><font size=10>to</font> <b><color=0xffffffff>Hornet EC-300[EVIL](Hornet EC-300)</b><font size=10><color=0x77ffffff> - Heavy Pulse Laser II - Hits",
		"[ 2026.11.04 12:03:05 ] (combat) Something the grammar has never seen",
		"",
	}, "\r")
	l, err := Read(strings.NewReader(log), "20261104_120000_90000001.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Fights) != 1 || l.Quiet != 1 {
		t.Fatalf("want the player fight alone and the rat fight dropped, got %d fights, %d quiet", len(l.Fights), l.Quiet)
	}
	f := l.Fights[0]
	if f.Listener != "Iwaira Itovuod" || f.CharacterID != 90000001 {
		t.Errorf("frame: %q %d", f.Listener, f.CharacterID)
	}
	if got := f.End.Sub(f.Start); got != 4*time.Second || len(f.Series.DamageOut) != 5 {
		t.Errorf("window %s, series %d seconds", got, len(f.Series.DamageOut))
	}
	if f.Series.DamageIn[1] != 120 || f.Series.DamageOut[1] != 523 || f.Series.RepsIn[3] != 352 || f.Series.CapIn[4] != 140 {
		t.Errorf("series: %+v", f.Series)
	}
	if len(f.Tackles) != 2 || f.Tackles[0].To != "you" || f.Tackles[1].From != "Crow" || f.Tackles[1].Kind != "disruption" {
		t.Errorf("tackles: %+v", f.Tackles)
	}
	if len(f.Slices) != 1 || f.Slices[0].At != 0 {
		t.Errorf("slices: %+v", f.Slices)
	}
	if f.Unparsed != 1 {
		t.Errorf("unparsed %d, want 1", f.Unparsed)
	}
	var bad, friend, drone *Tally
	for i := range f.Parties {
		switch f.Parties[i].Name {
		case "Bad Guy":
			bad = &f.Parties[i]
		case "Loki":
			friend = &f.Parties[i]
		case "Hornet EC-300":
			drone = &f.Parties[i]
		}
	}
	if bad == nil || friend == nil || drone == nil {
		t.Fatalf("parties: %+v", f.Parties)
	}
	if !bad.Pilot || bad.Hull != "Vexor" || bad.Ticker != "EVIL" || bad.DamageIn != 120 || bad.DamageOut != 523 {
		t.Errorf("Bad Guy: %+v", bad)
	}
	if friend.RepsIn != 352 || friend.Pilot {
		t.Errorf("Loki: %+v", friend)
	}
	if drone.Pilot {
		t.Errorf("a drone read as a pilot: %+v", drone)
	}
	var lasers *WeaponTally
	for i := range bad.Weapons {
		if bad.Weapons[i].Dir == DirOut && bad.Weapons[i].Weapon == "Heavy Pulse Laser II" {
			lasers = &bad.Weapons[i]
		}
	}
	if lasers == nil || lasers.Shots != 2 || lasers.Quality[QPenetrate] != 1 || lasers.Quality[QMiss] != 1 {
		t.Errorf("lasers: %+v", lasers)
	}
}
