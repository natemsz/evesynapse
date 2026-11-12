package fitview

import (
	"fmt"

	"evesynapse/internal/fit"
)

// FitFighterChip is a fighter line's launch report and its ability
// switches, shown under the line in the Fighters bay.
type FitFighterChip struct {
	Note      string // how many squadrons launch, and why any stay behind
	Held      bool   // some of the line stays in the hangar
	DPS       string // the launched squadrons' damage, "" when none
	Abilities []FitFighterAbility
}

// FitFighterAbility is one ability switch on a fighter line.
type FitFighterAbility struct {
	Key     string
	Name    string
	On      bool
	Default bool
	Damage  bool
	Prop    bool // a propulsion ability: one runs at a time
}

// FitFighterLimit is one of the Fighters bay's limits: launch tubes,
// a squadron class's slots, the hangar.
type FitFighterLimit struct {
	Label string
	Value string
	Full  bool // every tube or slot of the kind is taken
	Over  bool // more carried than fits
}

var fitFighterProp = map[string]bool{
	fit.FighterAbilityMWD: true, fit.FighterAbilityAB: true, fit.FighterAbilityEvasive: true, fit.FighterAbilityMJD: true,
}

// FitFighterAbilitiesOf is a document line's switched-on fighter
// abilities for the engine; nil for a line that is not a fighter.
func FitFighterAbilitiesOf(snap *fit.Snapshot, it FitDocItem) []string {
	if snap == nil || it.Cargo || snap.Attrs[it.TypeID][fit.AttrFighterSquadronMaxSize] <= 0 {
		return nil
	}
	return fit.FighterAbilitiesOn(snap, it.TypeID, it.AbilitiesOn, it.AbilitiesOff)
}

// sanitizeFighterAbilities keeps a line's ability lists to known
// keys, each once; a line in the hold carries none.
func sanitizeFighterAbilities(it *FitDocItem) {
	if it.Cargo {
		it.AbilitiesOn, it.AbilitiesOff = nil, nil
		return
	}
	known := map[string]bool{
		fit.FighterAbilityAttack: true, fit.FighterAbilityMissiles: true, fit.FighterAbilityBomb: true,
		fit.FighterAbilityMWD: true, fit.FighterAbilityAB: true, fit.FighterAbilityMJD: true, fit.FighterAbilityEvasive: true,
		fit.FighterAbilityECM: true, fit.FighterAbilityNeut: true, fit.FighterAbilityWeb: true,
		fit.FighterAbilityPoint: true, fit.FighterAbilityTackle: true,
	}
	clean := func(keys []string) []string {
		seen := map[string]bool{}
		var out []string
		for _, k := range keys {
			if known[k] && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		return out
	}
	it.AbilitiesOn = clean(it.AbilitiesOn)
	it.AbilitiesOff = clean(it.AbilitiesOff)
}

var fitFighterClassNames = map[string]string{"light": "light", "support": "support", "heavy": "heavy"}

// fitFighterChip builds a fighter line's report from the engine's
// launch report; nil when the engine has no line for the type.
func fitFighterChip(res *fit.Result, snap *fit.Snapshot, typeID int64) *FitFighterChip {
	if res == nil {
		return nil
	}
	var line *fit.FighterLine
	for i := range res.FighterLines {
		if res.FighterLines[i].TypeID == typeID {
			line = &res.FighterLines[i]
			break
		}
	}
	if line == nil {
		return nil
	}
	chip := &FitFighterChip{Held: line.Held != ""}
	class := fitFighterClassNames[line.Class]
	squads := "squadrons"
	if line.Squadrons == 1 {
		squads = "squadron"
	}
	var why string
	switch line.Held {
	case fit.FighterNoSlots:
		why = "this hull launches no " + class + " fighters"
	case fit.FighterSlotsFull:
		why = "the " + class + " slots are full"
	case fit.FighterTubesFull:
		why = "every launch tube is in use"
	}
	switch {
	case line.Squadrons == 0:
		chip.Note = "Stays in the hangar: " + why
	case line.Held != "":
		chip.Note = fmt.Sprintf("%d %s of %d launched, %d stay in the hangar: %s",
			line.Squadrons, squads, line.SquadronSize, line.Carried-line.Launched, why)
	default:
		chip.Note = fmt.Sprintf("%d %s of %d launched", line.Squadrons, squads, line.SquadronSize)
		if rem := line.Launched % line.SquadronSize; rem != 0 {
			chip.Note += fmt.Sprintf(", the last with %d", rem)
		}
	}
	if line.DPS > 0 {
		chip.DPS = FitFmt0(line.DPS) + " DPS"
	}
	on := map[string]bool{}
	for _, k := range line.Abilities {
		on[k] = true
	}
	for _, a := range fit.FighterAbilities(snap, typeID) {
		chip.Abilities = append(chip.Abilities, FitFighterAbility{
			Key: a.Key, Name: a.Name, On: on[a.Key], Default: a.Default, Damage: a.Damage, Prop: fitFighterProp[a.Key],
		})
	}
	return chip
}

// FitFighterLimits lists the Fighters bay's limits: launch tubes,
// each squadron class the hull launches or the fit carries, and the
// hangar.
func FitFighterLimits(res *fit.Result) []FitFighterLimit {
	if res == nil || (res.FighterTubes == 0 && res.FightersCarried == 0) {
		return nil
	}
	carried := map[string]bool{}
	for _, l := range res.FighterLines {
		carried[l.Class] = true
	}
	out := []FitFighterLimit{{Label: "Launch tubes", Value: fmt.Sprintf("%d/%d", res.SquadronsLaunched, res.FighterTubes),
		Full: res.FighterTubes > 0 && res.SquadronsLaunched >= res.FighterTubes}}
	for _, c := range []struct {
		key, label string
		used, max  int
	}{
		{"light", "Light", res.FighterLightUsed, res.FighterLightSlots},
		{"support", "Support", res.FighterSupportUsed, res.FighterSupportSlots},
		{"heavy", "Heavy", res.FighterHeavyUsed, res.FighterHeavySlots},
	} {
		if c.max == 0 && !carried[c.key] {
			continue
		}
		out = append(out, FitFighterLimit{Label: c.label, Value: fmt.Sprintf("%d/%d", c.used, c.max),
			Full: c.max > 0 && c.used >= c.max, Over: c.max == 0})
	}
	out = append(out, FitFighterLimit{Label: "Hangar",
		Value: FitFmt0(res.FighterHangarUsed) + " / " + FitFmt0(res.FighterHangarCapacity) + " m³",
		Over:  res.FighterHangarUsed > res.FighterHangarCapacity})
	return out
}
