package fit

import (
	"math"
	"sort"
	"strings"
)

// Fighter abilities: what a squadron can be told to do. Every
// ability is a dogma effect on the fighter type; the ones that deal
// damage feed DPS, volley and the range chart while switched on.
const (
	FighterAbilityAttack   = "attack"
	FighterAbilityMissiles = "missiles"
	FighterAbilityBomb     = "bomb"
	FighterAbilityMWD      = "mwd"
	FighterAbilityAB       = "ab"
	FighterAbilityMJD      = "mjd"
	FighterAbilityEvasive  = "evasive"
	FighterAbilityECM      = "ecm"
	FighterAbilityNeut     = "neut"
	FighterAbilityWeb      = "web"
	FighterAbilityPoint    = "point"
	FighterAbilityTackle   = "tackle"
)

// The ability effects not named with the propulsion set in engine.go.
const (
	EffectFighterMissiles = 6431
	EffectFighterNeut     = 6434
	EffectFighterWeb      = 6435
	EffectFighterPoint    = 6436
	EffectFighterECM      = 6437
	EffectFighterTackle   = 6464
	EffectFighterAttack   = 6465
	EffectFighterBomb     = 6485
)

// Fighter attributes beyond the squadron shape in engine.go: the
// missile ability's explosion, the bomb, and the structure (standup)
// squadron classes with their slots.
const (
	attrFighterMissilesExpRadius   = 2125
	attrFighterMissilesExpVel      = 2126
	attrFighterMissilesReduction   = 2127
	attrFighterMissilesSensitivity = 2128
	AttrFighterBombType            = 2324
	AttrFighterBombDuration        = 2349

	AttrFighterStandupLightSlots     = 2737
	AttrFighterStandupSupportSlots   = 2738
	AttrFighterStandupHeavySlots     = 2739
	AttrFighterSquadronIsStandupLite = 2740
	AttrFighterSquadronIsStandupSupp = 2741
	AttrFighterSquadronIsStandupHvy  = 2742
)

// The damage attributes a bomb carries (EM, thermal, kinetic,
// explosive), the same as any charge.
var bombDamage = []int64{114, 118, 117, 116}

// FighterAbility is one ability a fighter type has. Damage abilities
// count toward DPS; Default says whether a squadron starts with it
// switched on (damage and electronic warfare on, movement off).
type FighterAbility struct {
	Key     string
	Name    string
	Effect  int64
	Damage  bool
	Default bool
}

// fighterAbilityCatalog is every ability, in the order the workbench
// lists them: damage first, then movement, then electronic warfare.
var fighterAbilityCatalog = []FighterAbility{
	{FighterAbilityAttack, "Attack", EffectFighterAttack, true, true},
	{FighterAbilityMissiles, "Missiles", EffectFighterMissiles, true, true},
	{FighterAbilityBomb, "Bomb", EffectFighterBomb, true, true},
	{FighterAbilityMWD, "Microwarpdrive", EffectFighterMWD, false, false},
	{FighterAbilityAB, "Afterburner", EffectFighterAB, false, false},
	{FighterAbilityEvasive, "Evasive", EffectFighterEvasive, false, false},
	{FighterAbilityMJD, "Micro jump", EffectFighterMJD, false, false},
	{FighterAbilityECM, "ECM", EffectFighterECM, false, true},
	{FighterAbilityNeut, "Neutralizer", EffectFighterNeut, false, true},
	{FighterAbilityWeb, "Web", EffectFighterWeb, false, true},
	{FighterAbilityPoint, "Warp disruptor", EffectFighterPoint, false, true},
	{FighterAbilityTackle, "Tackle", EffectFighterTackle, false, true},
}

// fighterPropulsion are the abilities that move the squadron. A
// squadron runs one at a time, so switching one on switches the
// others off.
var fighterPropulsion = map[string]bool{
	FighterAbilityMWD: true, FighterAbilityAB: true, FighterAbilityEvasive: true, FighterAbilityMJD: true,
}

// FighterAbilities lists the abilities a fighter type has, in
// catalogue order; nil for a type that is not a fighter.
func FighterAbilities(snap *Snapshot, typeID int64) []FighterAbility {
	if snap == nil || snap.Attrs[typeID][AttrFighterSquadronMaxSize] <= 0 {
		return nil
	}
	var out []FighterAbility
	for _, a := range fighterAbilityCatalog {
		if snap.HasEffect(typeID, a.Effect) {
			out = append(out, a)
		}
	}
	return out
}

// FighterAbilitiesOn resolves which abilities a fighter line has
// switched on: the type's defaults, plus on, minus off. Keys the
// type does not have are ignored, and at most one propulsion
// ability stays on (the last one named in on wins). The result is
// in catalogue order.
func FighterAbilitiesOn(snap *Snapshot, typeID int64, on, off []string) []string {
	has := FighterAbilities(snap, typeID)
	set := make(map[string]bool, len(has))
	for _, a := range has {
		set[a.Key] = a.Default
	}
	prop := ""
	for _, k := range on {
		if _, ok := set[k]; ok {
			set[k] = true
			if fighterPropulsion[k] {
				prop = k
			}
		}
	}
	for _, k := range off {
		if _, ok := set[k]; ok {
			set[k] = false
		}
	}
	if prop != "" && set[prop] {
		for k := range fighterPropulsion {
			if k != prop {
				if _, ok := set[k]; ok {
					set[k] = false
				}
			}
		}
	}
	out := []string{}
	for _, a := range has {
		if set[a.Key] {
			out = append(out, a.Key)
		}
	}
	return out
}

// Why a fighter line launches fewer squadrons than it could fill.
const (
	// FighterNoSlots: the hull has no tubes for this class at all
	// (a carrier and heavy fighters, say).
	FighterNoSlots = "noslots"
	// FighterSlotsFull: earlier lines took every slot of the class.
	FighterSlotsFull = "slotsfull"
	// FighterTubesFull: every launch tube is in use.
	FighterTubesFull = "tubesfull"
)

// FighterLine is one fighter type's launch report: its class and
// squadron size, how many are carried, how many squadrons and
// fighters launch, why the rest stay in the hangar, which abilities
// are on, and the damage the launched squadrons do.
type FighterLine struct {
	TypeID       int64
	Class        string // "light", "support" or "heavy"
	SquadronSize int
	Carried      int
	Squadrons    int // launched
	Launched     int // fighters in the launched squadrons
	Held         string
	Abilities    []string // switched on, catalogue order
	DPS          float64
	Volley       float64
}

// fighterClass reads a fighter's class and whether it is a
// structure (standup) fighter.
func fighterClass(base map[int64]float64) (class string, standup bool) {
	switch {
	case base[AttrFighterSquadronIsStandupHvy] > 0:
		return "heavy", true
	case base[AttrFighterSquadronIsStandupSupp] > 0:
		return "support", true
	case base[AttrFighterSquadronIsStandupLite] > 0:
		return "light", true
	case base[AttrFighterSquadronIsHeavy] > 0:
		return "heavy", false
	case base[AttrFighterSquadronIsSupport] > 0:
		return "support", false
	}
	return "light", false
}

// fighterAbilityKey is the canonical form of an ability set, the
// part of a fighter entity's identity that keeps lines with
// different abilities apart.
func fighterAbilityKey(on []string) string {
	keys := append([]string(nil), on...)
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// fighterStats launches the hangar's fighters, a squadron per tube
// within the hull's light, support and heavy slots (the standup
// slots on a structure), in the order they were added, and totals
// what the launched squadrons' switched-on damage abilities do:
// the sustained attack, the missile salvo and the bomb. Missiles
// and bombs have limited charges in the game; their figures are
// the rate while the charges last.
func (f *fitting) fighterStats() {
	snap, res := f.snap, f.res
	res.FighterTubes = int(f.shipAttr(AttrFighterTubes))
	res.FighterLightSlots = int(f.shipAttr(AttrFighterLightSlots))
	res.FighterSupportSlots = int(f.shipAttr(AttrFighterSupportSlots))
	res.FighterHeavySlots = int(f.shipAttr(AttrFighterHeavySlots))
	standupSlots := [3]int{int(f.shipAttr(AttrFighterStandupLightSlots)), int(f.shipAttr(AttrFighterStandupSupportSlots)), int(f.shipAttr(AttrFighterStandupHeavySlots))}
	if standupSlots[0]+standupSlots[1]+standupSlots[2] > 0 {
		res.FighterStandup = true
		res.FighterLightSlots, res.FighterSupportSlots, res.FighterHeavySlots = standupSlots[0], standupSlots[1], standupSlots[2]
	}
	res.FighterHangarCapacity = f.shipAttr(AttrFighterCapacity)
	tubes := res.FighterTubes
	for _, ent := range f.fighters {
		carried := ent.instances
		res.FightersCarried += carried
		if phys, ok := snap.Physics[ent.typeID]; ok {
			res.FighterHangarUsed += phys.Volume * float64(carried)
		}
		size := int(ent.get(snap, AttrFighterSquadronMaxSize))
		if size < 1 {
			size = 1
		}
		class, standup := fighterClass(ent.base)
		line := FighterLine{TypeID: ent.typeID, Class: class, SquadronSize: size, Carried: carried, Abilities: ent.abilities}
		on := make(map[string]bool, len(ent.abilities))
		for _, k := range ent.abilities {
			on[k] = true
		}
		var used *int
		var slots int
		switch class {
		case "heavy":
			used, slots = &res.FighterHeavyUsed, res.FighterHeavySlots
		case "support":
			used, slots = &res.FighterSupportUsed, res.FighterSupportSlots
		default:
			used, slots = &res.FighterLightUsed, res.FighterLightSlots
		}
		if standup != res.FighterStandup {
			slots = 0 // a structure fighter on a ship, or the reverse
		}

		var attackVolley, attackCycle, salvo, salvoCycle, bomb, bombCycle float64
		if on[FighterAbilityAttack] {
			attackVolley, attackCycle = fighterAbility(ent, snap, fighterAttackMissileDamage, AttrFighterAttackMissileMult, AttrFighterAttackMissileDuration)
			if attackVolley == 0 {
				attackVolley, attackCycle = fighterAbility(ent, snap, fighterAttackTurretDamage, AttrFighterAttackTurretMult, AttrFighterAttackTurretDuration)
			}
		}
		if on[FighterAbilityMissiles] {
			salvo, salvoCycle = fighterAbility(ent, snap, fighterMissilesDamage, AttrFighterMissilesMult, AttrFighterMissilesDuration)
		}
		bombType := int64(ent.get(snap, AttrFighterBombType))
		if on[FighterAbilityBomb] && bombType > 0 {
			for _, a := range bombDamage {
				bomb += snap.Attrs[bombType][a]
			}
			bombCycle = ent.get(snap, AttrFighterBombDuration) / 1000
		}

		for left := carried; left > 0; left -= size {
			switch {
			case slots == 0:
				line.Held = FighterNoSlots
			case *used >= slots:
				line.Held = FighterSlotsFull
			case tubes <= 0:
				line.Held = FighterTubesFull
			}
			if line.Held != "" {
				break
			}
			members := min(size, left)
			line.Launched += members
			line.Squadrons++
			tubes--
			*used++
			res.SquadronsLaunched++
		}
		members := float64(line.Launched)
		if members == 0 {
			res.FighterLines = append(res.FighterLines, line)
			continue
		}
		reach := func(dps float64, radius, velocity, reduction, sensitivity float64) {
			w := WeaponReach{TypeID: ent.typeID, Kind: ReachFighter, Count: line.Launched, DPS: dps,
				ExplosionRadius: radius, ExplosionVelocity: velocity}
			if reduction > 0 && sensitivity > 1 {
				w.DamageReduction = math.Log(reduction) / math.Log(sensitivity)
			}
			res.Weapons = append(res.Weapons, w)
		}
		if attackCycle > 0 && attackVolley > 0 {
			dps := members * attackVolley / attackCycle
			line.DPS += dps
			line.Volley += members * attackVolley
			reach(dps, ent.get(snap, attrFighterAttackExpRadius), ent.get(snap, attrFighterAttackExpVel),
				ent.get(snap, attrFighterAttackReduction), ent.get(snap, attrFighterAttackSensitivity))
		}
		if salvoCycle > 0 && salvo > 0 {
			dps := members * salvo / salvoCycle
			line.DPS += dps
			line.Volley += members * salvo
			res.FighterSalvo += members * salvo
			res.FighterSalvoSeconds = math.Max(res.FighterSalvoSeconds, salvoCycle)
			reach(dps, ent.get(snap, attrFighterMissilesExpRadius), ent.get(snap, attrFighterMissilesExpVel),
				ent.get(snap, attrFighterMissilesReduction), ent.get(snap, attrFighterMissilesSensitivity))
		}
		if bombCycle > 0 && bomb > 0 {
			// A bomb's blast hits whatever sits in it, scaled by
			// signature against the blast's size; speed does not
			// matter.
			dps := members * bomb / bombCycle
			line.DPS += dps
			line.Volley += members * bomb
			reach(dps, snap.Attrs[bombType][attrAoeCloudSize], 0, 0, 0)
		}
		res.FighterDPS += line.DPS
		res.FighterVolley += line.Volley
		res.FighterLines = append(res.FighterLines, line)
	}
}

// fighterAbility is one fighter's damage per activation of an
// ability and the activation's cycle in seconds.
func fighterAbility(ent *entity, snap *Snapshot, damage []int64, mult, duration int64) (volley, cycle float64) {
	for _, a := range damage {
		volley += ent.get(snap, a)
	}
	return volley * ent.get(snap, mult), ent.get(snap, duration) / 1000
}

// fighterOnlyListed keeps the resolved abilities that listed names:
// an explicit list is the whole set, so a default it leaves out is
// switched off.
func fighterOnlyListed(resolved, listed []string) []string {
	want := make(map[string]bool, len(listed))
	for _, k := range listed {
		want[k] = true
	}
	out := resolved[:0:0]
	for _, k := range resolved {
		if want[k] {
			out = append(out, k)
		}
	}
	return out
}
