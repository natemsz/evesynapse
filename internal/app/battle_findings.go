package app

// ---------------------------------------------------------------------------
// Battle reports, findings: a deterministic rule engine over a fight's
// computed facts. Every finding is one plain sentence with its numbers
// in it, made by a named rule with its thresholds below. No rule says
// anything the numbers do not show, and none runs on a guess: a rule
// whose inputs are missing stays silent.
// ---------------------------------------------------------------------------

import (
	"fmt"
	"sort"
	"strings"

	db "evesynapse/internal/db/sqlc"
)

// The rule pack's thresholds, in one place.
const (
	findApplicationMinShots = 10   // a weapon's shots at one target before application is judged
	findApplicationPoorPct  = 40   // grazes plus misses, percent of shots, that make application poor
	findApplicationMaxRows  = 3    // poor-application findings at most
	findWastedMinDamage     = 1000 // damage into survivors worth a finding
	findContributionRows    = 3    // kills whose share is named one by one
	findTackleMaxRows       = 3    // tacklers named at most
)

type battleFacts struct {
	Fight         db.BattleFight
	Series        battleSeries
	Parties       []db.BattleFightParty
	Weapons       []db.BattleFightWeapon
	Kills         []db.BattleFightKill
	Tackles       []db.BattleFightTackle
	ShipCapacitor float64 // the lost ship's capacitor in GJ, 0 when unknown
}

type battleFinding struct {
	Rule string
	Text string
}

func pct(part, whole int64) int64 {
	if whole <= 0 {
		return 0
	}
	return (part*100 + whole/2) / whole
}

func partyLabel(p db.BattleFightParty) string {
	if p.Hull != "" {
		return fmt.Sprintf("%s (%s)", p.Name, p.Hull)
	}
	return p.Name
}

// battleFindings runs the rule pack in a fixed order.
func battleFindings(x battleFacts) []battleFinding {
	var out []battleFinding
	add := func(rule, format string, args ...any) {
		out = append(out, battleFinding{Rule: rule, Text: fmt.Sprintf(format, args...)})
	}
	var loss *db.BattleFightKill
	for i := range x.Kills {
		if x.Kills[i].Loss {
			loss = &x.Kills[i]
			break
		}
	}

	// Focus: who put the damage on you.
	if x.Fight.DamageIn > 0 {
		var shooters []db.BattleFightParty
		for _, p := range x.Parties {
			if p.DamageIn > 0 {
				shooters = append(shooters, p)
			}
		}
		sort.SliceStable(shooters, func(i, j int) bool { return shooters[i].DamageIn > shooters[j].DamageIn })
		if len(shooters) > 0 {
			top := shooters[0]
			if len(shooters) == 1 {
				add("focus", "All %s damage you took came from %s.", battleNum(x.Fight.DamageIn), partyLabel(top))
			} else {
				add("focus", "%d attackers shot you; %s dealt %d%% of the %s damage you took.",
					len(shooters), partyLabel(top), pct(top.DamageIn, x.Fight.DamageIn), battleNum(x.Fight.DamageIn))
			}
		}
		if x.Fight.PeakInDps > 0 {
			add("focus", "Incoming damage peaked at %s a second (10-second average) at %s.",
				battleNum(x.Fight.PeakInDps), clockOffset(int(x.Fight.PeakInAt)))
		}
	}

	// Application: weapons whose shots mostly grazed or missed.
	bySeq := map[int32]db.BattleFightParty{}
	for _, p := range x.Parties {
		bySeq[p.Seq] = p
	}
	type poor struct {
		w   db.BattleFightWeapon
		bad int64
	}
	var poors []poor
	for _, w := range x.Weapons {
		if w.Dir != "out" {
			continue
		}
		judged := w.QMiss + w.QGraze + w.QGlance + w.QHit + w.QPenetrate + w.QSmash + w.QWreck
		bad := w.QMiss + w.QGraze
		if judged >= findApplicationMinShots && pct(bad, judged) >= findApplicationPoorPct {
			poors = append(poors, poor{w, bad})
		}
	}
	sort.SliceStable(poors, func(i, j int) bool { return poors[i].w.Shots > poors[j].w.Shots })
	for i, p := range poors {
		if i == findApplicationMaxRows {
			break
		}
		judged := p.w.QMiss + p.w.QGraze + p.w.QGlance + p.w.QHit + p.w.QPenetrate + p.w.QSmash + p.w.QWreck
		add("application", "%d of %d %s shots at %s grazed or missed (%d%%).",
			p.bad, judged, p.w.Weapon, bySeq[p.w.PartySeq].Name, pct(p.bad, judged))
	}

	// Wasted damage: pilots shot whom no matched mail shows dying.
	// The rule speaks only when every matched mail's victim has a
	// known name, so a name not yet resolved never reads as an
	// escape.
	named := true
	for _, k := range x.Kills {
		if k.VictimCharacterID > 0 && k.VictimName == "" {
			named = false
		}
	}
	var wasted int64
	var survivors []db.BattleFightParty
	for _, p := range x.Parties {
		if p.Pilot && !p.Died && p.DamageOut > 0 {
			wasted += p.DamageOut
			survivors = append(survivors, p)
		}
	}
	if named && wasted >= findWastedMinDamage && x.Fight.DamageOut > 0 {
		sort.SliceStable(survivors, func(i, j int) bool { return survivors[i].DamageOut > survivors[j].DamageOut })
		add("wasted", "%s of your %s damage (%d%%) went into pilots no matched killmail shows dying, %s %s of it.",
			battleNum(wasted), battleNum(x.Fight.DamageOut), pct(wasted, x.Fight.DamageOut),
			partyLabel(survivors[0]), battleNum(survivors[0].DamageOut))
	}

	// Tank verdict: on a loss, when incoming damage passed incoming
	// repairs for good.
	if loss != nil && len(x.Series.DamageIn) > 0 {
		in, reps := rolling(x.Series.DamageIn), rolling(x.Series.RepsIn)
		end := min(max(int(loss.AtSecond), 0), len(in)-1)
		if x.Fight.RepsIn == 0 {
			add("tank", "You received no remote repairs; you died at %s.", clockOffset(int(loss.AtSecond)))
		} else {
			cross := -1
			for i := end; i >= 0; i-- {
				if in[i] <= reps[i] {
					break
				}
				cross = i
			}
			if cross >= 0 {
				add("tank", "From %s incoming damage outpaced your incoming repairs (%s against %s a second) and stayed ahead; you lasted %d seconds after that.",
					clockOffset(cross), battleNum(int64(in[cross])), battleNum(int64(reps[cross])), end-cross)
			}
		}
	}

	// Cap pressure.
	if x.Fight.CapIn > 0 {
		peak, at := peakIncoming(x.Series.CapIn)
		add("cap", "Energy warfare took %s GJ from you, peaking at %s GJ a second at %s (10-second average).",
			battleNum(x.Fight.CapIn), battleNum(peak), clockOffset(at))
		if loss != nil && x.ShipCapacitor > 0 {
			var before int64
			for i := 0; i <= int(loss.AtSecond) && i < len(x.Series.CapIn); i++ {
				before += x.Series.CapIn[i]
			}
			if float64(before) >= x.ShipCapacitor {
				add("cap", "Estimate: the %s GJ neutralized before your death is more than your hull's %s GJ base capacitor, so you were likely dry.",
					battleNum(before), battleNum(int64(x.ShipCapacitor)))
			}
		}
	}

	// Contribution: your share of the damage on the kills you are
	// on, the biggest shares named, the rest as a range.
	var mine []db.BattleFightKill
	for _, k := range x.Kills {
		if !k.Loss && k.DamageTaken > 0 && k.MyDamage > 0 {
			mine = append(mine, k)
		}
	}
	sort.SliceStable(mine, func(i, j int) bool {
		return pct(mine[i].MyDamage, mine[i].DamageTaken) > pct(mine[j].MyDamage, mine[j].DamageTaken)
	})
	if len(mine) > findContributionRows {
		lo, hi := pct(mine[len(mine)-1].MyDamage, mine[len(mine)-1].DamageTaken), pct(mine[0].MyDamage, mine[0].DamageTaken)
		add("contribution", "You were on %d kills; your share of the damage ran from %d%% to %d%%.", len(mine), lo, hi)
	}
	for i, k := range mine {
		if i == findContributionRows {
			break
		}
		who := k.VictimName
		if who == "" {
			who = "the victim"
		}
		add("contribution", "You dealt %s of the %s damage on %s's kill (%d%%).",
			battleNum(k.MyDamage), battleNum(k.DamageTaken), who, pct(k.MyDamage, k.DamageTaken))
	}

	// Tackle: who held you (the log says "you" for the pilot),
	// against your death or the fight's end; and how much tackle
	// the grid saw besides.
	held, grid := 0, 0
	for _, t := range x.Tackles {
		if !strings.EqualFold(t.ToName, "you") {
			if !strings.EqualFold(t.FromName, "you") {
				grid++
			}
			continue
		}
		if held == findTackleMaxRows {
			continue
		}
		held++
		at := int(t.FirstAt.Sub(x.Fight.StartedAt).Seconds())
		kind := "warp scrambled"
		if t.Kind == "disruption" {
			kind = "warp disrupted"
		}
		tries := fmt.Sprintf("%d attempts", t.Attempts)
		if t.Attempts == 1 {
			tries = "1 attempt"
		}
		if loss != nil {
			add("tackle", "%s %s you from %s (%s), %d seconds before you died.", t.FromName, kind, clockOffset(at), tries, int(loss.AtSecond)-at)
		} else {
			add("tackle", "%s %s you from %s (%s); you did not die.", t.FromName, kind, clockOffset(at), tries)
		}
	}
	if grid > 0 {
		add("tackle", "The grid around you saw %d other tackle %s.", grid, map[bool]string{true: "pairing", false: "pairings"}[grid == 1])
	}
	return out
}
