package app

// Calibration tests for the fitting engine. Fixtures use real
// SDE values copied from the live Fuzzwork dump (2026-10-04), so
// every expected number can be re-derived by hand from the data;
// the arithmetic is spelled out next to each assertion.

import (
	"context"
	"math"
	"testing"
)

// fitFixture builds an in-memory fitSnapshot for engine tests.
type fitFixture struct {
	snap *fitSnapshot
}

func newFitFixture() *fitFixture {
	return &fitFixture{snap: &fitSnapshot{
		attrs:        map[int64]map[int64]float64{},
		physics:      map[int64]fitPhysics{},
		groups:       map[int64]int64{},
		meta:         map[int64]fitAttrMeta{},
		effects:      map[int64]*fitEffect{},
		typeEffects:  map[int64][]int64{},
		requirements: map[int64][]fitSkillReq{},
	}}
}

func (f *fitFixture) attrs(typeID int64, attrs map[int64]float64) *fitFixture {
	f.snap.attrs[typeID] = attrs
	return f
}

func (f *fitFixture) group(typeID, groupID int64) *fitFixture {
	f.snap.groups[typeID] = groupID
	return f
}

func (f *fitFixture) meta(attr int64, stackable bool, def float64) *fitFixture {
	f.snap.meta[attr] = fitAttrMeta{Stackable: stackable, Default: def}
	return f
}

func (f *fitFixture) phys(typeID int64, p fitPhysics) *fitFixture {
	f.snap.physics[typeID] = p
	return f
}

func (f *fitFixture) requires(typeID int64, reqs ...fitSkillReq) *fitFixture {
	f.snap.requirements[typeID] = append(f.snap.requirements[typeID], reqs...)
	return f
}

// effect attaches an effect (with modifiers) to a type.
func (f *fitFixture) effect(typeID, effectID int64, category int64, mods ...fitModifier) *fitFixture {
	f.snap.typeEffects[typeID] = append(f.snap.typeEffects[typeID], effectID)
	f.snap.effects[effectID] = &fitEffect{Category: category, Modifiers: mods}
	return f
}

// flag attaches a no-modifier effect (slot markers etc.) to a type.
func (f *fitFixture) flag(typeID, effectID int64) *fitFixture {
	f.snap.typeEffects[typeID] = append(f.snap.typeEffects[typeID], effectID)
	return f
}

func fitMod(domain, fn string, modified, modifying, op int64) fitModifier {
	return fitModifier{Domain: domain, Func: fn, ModifiedAttr: modified, ModifyingAttr: modifying, Operation: op}
}

func fitModGroup(modified, modifying, op, groupID int64) fitModifier {
	return fitModifier{Domain: "shipID", Func: "LocationGroupModifier", ModifiedAttr: modified, ModifyingAttr: modifying, Operation: op, GroupID: groupID}
}

func fitModSkill(modified, modifying, op, skillTypeID int64) fitModifier {
	return fitModifier{Domain: "shipID", Func: "LocationRequiredSkillModifier", ModifiedAttr: modified, ModifyingAttr: modifying, Operation: op, SkillTypeID: skillTypeID}
}

func near(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s = %v, want %v (+/- %v)", what, got, want, tol)
	}
}

// TestFitEngineOperationSemantics pins the dogma operation
// mapping at the fold level (see fitengine.go's header for the
// dump evidence behind each code).
func TestFitEngineOperationSemantics(t *testing.T) {
	const ship = int64(1001)

	t.Run("ModAdd adds flat hit points (plate, effect 2837)", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{265: 500})
		fx.attrs(2001, map[int64]float64{1159: 1200})
		fx.effect(2001, 2837, 4, fitMod("shipID", "ItemModifier", 265, 1159, 2))
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2001}}, nil, nil)
		near(t, "armorHP", res.ArmorHP, 1700, 0.001)
	})

	t.Run("skill level pre-multiplies the ship bonus, then percent applies (Punisher/Maller chain)", func(t *testing.T) {
		fx := newFitFixture()
		// Ship: armor EM resonance 0.5, per-level bonus -4.
		fx.attrs(ship, map[int64]float64{267: 0.5, 464: -4})
		// Ship effect 1804: resonance percent by shipBonusAF.
		fx.effect(ship, 1804, 0, fitMod("shipID", "ItemModifier", 267, 464, 6))
		// Skill 2005 ("frigate skill"): effect pre-multiplies the
		// ship's bonus attribute by skillLevel (real effect 510
		// shape on Amarr Frigate).
		fx.attrs(2005, map[int64]float64{})
		fx.effect(2005, 510, 0, fitMod("shipID", "ItemModifier", 464, 280, 0))
		res := computeFit(fx.snap, ship, nil, map[int64]int{2005: 5}, nil)
		// -4 * 5 = -20 -> 0.5 * (1 - 20/100) = 0.40.
		near(t, "armor EM resonance", res.ShipAttrs[267], 0.4, 1e-9)
	})

	t.Run("stacking penalty damps the second gyro (eos curve)", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{64: 2.0})
		fx.meta(64, false, 1)
		for _, mod := range []int64{2002, 2003} {
			fx.attrs(mod, map[int64]float64{64: 1.1})
			fx.effect(mod, 9001, 4, fitMod("shipID", "ItemModifier", 64, 64, 4))
		}
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2002}, {TypeID: 2003}}, nil, nil)
		// 2.0 * 1.1 * (1 + 0.1 * exp(-1/7.1289)) = 2.3912...
		want := 2.0 * 1.1 * (1 + 0.1*math.Exp(-1.0/7.1289))
		near(t, "stacked multiplier", res.ShipAttrs[64], want, 1e-6)
	})

	t.Run("bonuses and penalties penalize in separate chains", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{64: 2.0})
		fx.meta(64, false, 1)
		fx.attrs(2004, map[int64]float64{64: 1.21})
		fx.effect(2004, 9002, 4, fitMod("shipID", "ItemModifier", 64, 64, 4))
		fx.attrs(2005, map[int64]float64{64: 0.8})
		fx.effect(2005, 9003, 4, fitMod("shipID", "ItemModifier", 64, 64, 4))
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2004}, {TypeID: 2005}}, nil, nil)
		// Each is first in its own chain: both at full strength.
		near(t, "bonus/penalty chains", res.ShipAttrs[64], 2.0*1.21*0.8, 1e-9)
	})

	t.Run("percent hardeners stack-penalize as penalties", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{267: 0.5})
		fx.meta(267, false, 1)
		for _, mod := range []int64{2006, 2007} {
			fx.attrs(mod, map[int64]float64{9001: -30})
			fx.effect(mod, 9004, 4, fitMod("shipID", "ItemModifier", 267, 9001, 6))
		}
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2006}, {TypeID: 2007}}, nil, nil)
		// 0.5 * (1-0.30) * (1 - 0.30*exp(-1/7.1289)).
		want := 0.5 * 0.7 * (1 - 0.3*math.Exp(-1.0/7.1289))
		near(t, "hardener stacking", res.ShipAttrs[267], want, 1e-6)
	})

	t.Run("pre-multiply lands before post-percent (Damage Control shape)", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{267: 0.5})
		fx.meta(267, false, 1)
		fx.attrs(2008, map[int64]float64{267: 0.85})
		// Real effect 2302: resonance pre-multiplied by the
		// module's own resonance attribute (op 0).
		fx.effect(2008, 2302, 4, fitMod("shipID", "ItemModifier", 267, 267, 0))
		fx.attrs(2009, map[int64]float64{9001: -20})
		fx.effect(2009, 9005, 4, fitMod("shipID", "ItemModifier", 267, 9001, 6))
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2008}, {TypeID: 2009}}, nil, nil)
		// (0.5 * 0.85) * (1 - 20/100) = 0.34.
		near(t, "premul-then-percent", res.ShipAttrs[267], 0.34, 1e-9)
	})

	t.Run("assign / subtract / divide operations", func(t *testing.T) {
		fx := newFitFixture()
		fx.attrs(ship, map[int64]float64{192: 2, 714: 100, 76: 20000})
		fx.meta(192, true, 0)
		// PostAssign (op 7, real effect 2886 setMaxLockedTargets).
		fx.attrs(2010, map[int64]float64{235: 7})
		fx.effect(2010, 2886, 0, fitMod("shipID", "ItemModifier", 192, 235, 7))
		// ModSub (op 3, real effect 1650 consumption quantity).
		fx.attrs(2011, map[int64]float64{885: 30})
		fx.effect(2011, 1650, 0, fitModSkill(714, 885, 3, 0))
		// PostDiv (op 5, real effect 6010 shipMode MaxTargetRange).
		fx.attrs(2012, map[int64]float64{1991: 2})
		fx.effect(2012, 6010, 0, fitMod("shipID", "ItemModifier", 76, 1991, 5))
		res := computeFit(fx.snap, ship, []fitItemInput{{TypeID: 2010}, {TypeID: 2011}, {TypeID: 2012}}, nil, nil)
		near(t, "maxLockedTargets", res.ShipAttrs[192], 7, 1e-9)
		near(t, "maxTargetRange postdiv", res.ShipAttrs[76], 10000, 1e-9)
	})
}

// punisherFixture assembles the canonical frigate fit from real
// SDE values (dump of 2026-10-04; type IDs and numbers are the
// game's own).
func punisherFixture() *fitFixture {
	fx := newFitFixture()

	// Punisher (597). Resonances EM/TH/KN/EX as in the dump.
	fx.attrs(597, map[int64]float64{
		9: 450, 11: 67, 12: 5, 13: 2, 14: 4, 37: 355, 48: 140,
		55: 160000, 70: 2.9, 76: 25000, 101: 0, 102: 4,
		109: 0.67, 110: 0.67, 111: 0.67, 113: 0.67,
		192: 5, 208: 10,
		263: 350, 265: 500,
		267: 0.5, 268: 0.8, 269: 0.75, 270: 0.65,
		271: 1.0, 272: 0.5, 273: 0.6, 274: 0.8,
		479: 625000, 482: 400, 552: 37, 564: 640,
		1132: 400, 1137: 3,
	})
	fx.phys(597, fitPhysics{Mass: 1190000, Volume: 28600, Capacity: 275})

	// 200mm AutoCannon II (2889), group 55.
	fx.attrs(2889, map[int64]float64{9: 40, 30: 4, 50: 9, 51: 3750, 64: 3.465, 6: 0, 160: 315})
	fx.group(2889, 55)
	fx.flag(2889, 12).flag(2889, 42).flag(2889, 16)
	fx.requires(2889, fitSkillReq{SkillTypeID: 3300, Level: 1}, fitSkillReq{SkillTypeID: 3302, Level: 3})

	// Gyrostabilizer II (519).
	fx.attrs(519, map[int64]float64{9: 40, 30: 1, 50: 30, 64: 1.1, 204: 0.895})
	fx.group(519, 59)
	fx.flag(519, 11).flag(519, 16)
	fx.effect(519, 89, 4, fitModGroup(51, 204, 4, 55))
	fx.effect(519, 92, 4, fitModGroup(64, 64, 4, 55))

	// 400mm Steel Plates II (20349).
	fx.attrs(20349, map[int64]float64{9: 40, 30: 35, 50: 23, 796: 375000, 1159: 1200})
	fx.group(20349, 329)
	fx.flag(20349, 11).flag(20349, 16)
	fx.effect(20349, 1959, 4, fitMod("shipID", "ItemModifier", 4, 796, 2))
	fx.effect(20349, 2837, 4, fitMod("shipID", "ItemModifier", 265, 1159, 2))

	// Small Shield Extender I (377).
	fx.attrs(377, map[int64]float64{9: 40, 30: 2, 50: 20, 72: 400, 983: 2})
	fx.group(377, 38)
	fx.flag(377, 13).flag(377, 16)
	fx.effect(377, 21, 4, fitMod("shipID", "ItemModifier", 263, 72, 2))
	fx.effect(377, 2029, 4, fitMod("shipID", "ItemModifier", 552, 983, 2))

	// Small Shield Booster II (400).
	fx.attrs(400, map[int64]float64{9: 40, 30: 3, 50: 29, 6: 20, 68: 35, 73: 2000})
	fx.group(400, 40)
	fx.flag(400, 13).flag(400, 16)
	fx.flag(400, 4)

	// EMP S (185): EM 9 / EX 2 / KN 1.
	fx.attrs(185, map[int64]float64{114: 9, 116: 2, 117: 1, 118: 0, 120: 0.5, 244: 1})
	fx.group(185, 83)
	fx.effect(185, 596, 0, fitMod("otherID", "ItemModifier", 54, 120, 0))
	fx.effect(185, 600, 0, fitMod("otherID", "ItemModifier", 160, 244, 4))

	// Gunnery (3300): -2% turret cycle per level; applications
	// mirror real effects 413/414. Effect 132 (skillEffect) is
	// included to prove skillLevel stays an engine input.
	fx.attrs(3300, map[int64]float64{441: -2})
	fx.effect(3300, 132, 0,
		fitMod("itemID", "ItemModifier", 280, 276, 2),
		fitMod("itemID", "ItemModifier", 280, 275, 9))
	fx.effect(3300, 413, 0, fitMod("itemID", "ItemModifier", 441, 280, 0))
	fx.effect(3300, 414, 0, fitModSkill(51, 441, 6, 3300))

	// Small Projectile Turret (3302): +5% damage per level
	// (modeled on real effects 152/157 of Large Hybrid Turret).
	fx.attrs(3302, map[int64]float64{292: 5})
	fx.effect(3302, 152, 0, fitMod("itemID", "ItemModifier", 292, 280, 0))
	fx.effect(3302, 157, 0, fitModSkill(64, 292, 6, 3302))

	// Attribute metadata the fold needs.
	fx.meta(64, false, 1) // damageMultiplier: stacking-penalized
	fx.meta(51, false, 0) // speed (cycle)
	fx.meta(263, true, 0) // shieldCapacity
	fx.meta(265, true, 0) // armorHP
	for _, a := range []int64{267, 268, 269, 270, 271, 272, 273, 274, 109, 110, 111, 113} {
		fx.meta(a, false, 1) // resonances
	}
	return fx
}

// TestFitEngineCanonicalPunisherFit is the pyfa-style canonical
// check: a complete small fit whose headline numbers are worked
// by hand from the SDE fixture values.
func TestFitEngineCanonicalPunisherFit(t *testing.T) {
	fx := punisherFixture()
	items := []fitItemInput{
		{TypeID: 2889}, {TypeID: 2889}, {TypeID: 2889},
		{TypeID: 519}, {TypeID: 20349}, {TypeID: 377}, {TypeID: 400},
	}
	res := computeFit(fx.snap, 597, items, map[int64]int{3300: 5, 3302: 5},
		map[int64]int64{2889: 185})

	// Resources: PG 3*4 + 1 + 35 + 2 + 3 = 53 of 67;
	// CPU 3*9 + 30 + 23 + 20 + 29 = 129 of 140.
	near(t, "PG used", res.PowergridUsed, 53, 1e-9)
	near(t, "PG max", res.PowergridMax, 67, 1e-9)
	near(t, "CPU used", res.CPUUsed, 129, 1e-9)
	near(t, "CPU max", res.CPUMax, 140, 1e-9)

	// Slots: 3/4 high, 2/2 med (extender + booster), 2/5 low
	// (gyro + plate); turret hardpoints 3/4.
	if res.HighSlotsUsed != 3 || res.HighSlots != 4 ||
		res.MediumSlotsUsed != 2 || res.MediumSlots != 2 ||
		res.LowSlotsUsed != 2 || res.LowSlots != 5 ||
		res.TurretHardpointsUsed != 3 || res.TurretHardpoints != 4 {
		t.Fatalf("slots = hi %d/%d med %d/%d low %d/%d turret %d/%d",
			res.HighSlotsUsed, res.HighSlots, res.MediumSlotsUsed, res.MediumSlots,
			res.LowSlotsUsed, res.LowSlots, res.TurretHardpointsUsed, res.TurretHardpoints)
	}

	// Turret DPS, by hand:
	//   damage multiplier = 3.465 * 1.1 (gyro, first in chain)
	//                       * 1.25 (Small Projectile Turret V:
	//                       +5%/level pre-multiplied to +25)
	//   cycle = 3750ms * 0.895 (gyro) * 0.90 (Gunnery V: -2%/level)
	//   volley = (9+2+1) = 12 charge damage * multiplier
	//   DPS = 3 turrets * volley / cycle
	wantDPS := 3 * (12 * (3.465 * 1.1 * 1.25)) / (3750 * 0.895 * 0.90 / 1000)
	near(t, "turret DPS", res.TurretDPS, wantDPS, 0.001)
	near(t, "total DPS", res.DPS, wantDPS, 0.001)

	// HP: shield 350 + 400 (extender) = 750; armor 500 + 1200
	// (plate) = 1700; hull 450.
	near(t, "shield HP", res.ShieldHP, 750, 1e-9)
	near(t, "armor HP", res.ArmorHP, 1700, 1e-9)
	near(t, "hull HP", res.HullHP, 450, 1e-9)

	// EHP (omni = HP / mean resonance):
	//   shield mean = (1.0+0.8+0.6+0.5)/4 = 0.725 -> 1034.48
	//   armor  mean = (0.5+0.65+0.75+0.8)/4 = 0.675 -> 2518.52
	//   hull   450 / 0.67 -> 671.64
	near(t, "shield EHP", res.ShieldEHP.Omni, 750/0.725, 0.01)
	near(t, "armor EHP", res.ArmorEHP.Omni, 1700/0.675, 0.01)
	near(t, "hull EHP", res.HullEHP.Omni, 450/0.67, 0.01)
	near(t, "total EHP", res.EHP.Omni, 750/0.725+1700/0.675+450/0.67, 0.02)
	// Per-type spot check: shield vs pure EM = 750 / 1.0.
	near(t, "shield EM EHP", res.ShieldEHP.EM, 750, 0.01)

	// Tank rates: shield regen peak 2.5*750/625 = 3.0 HP/s;
	// booster 35 HP / 2 s = 17.5 HP/s.
	near(t, "shield regen peak", res.ShieldRegenPeak, 3.0, 1e-9)
	near(t, "shield boost rate", res.ShieldBoostRate, 17.5, 1e-9)

	// Capacitor: 400 GJ, 160 s recharge -> peak 6.25 GJ/s.
	// Booster draws 20/2 = 10 GJ/s > peak -> unstable.
	near(t, "cap capacity", res.CapacitorCapacity, 400, 1e-9)
	near(t, "cap peak", res.CapacitorPeakRecharge, 6.25, 1e-9)
	near(t, "cap draw", res.CapacitorDraw, 10, 1e-9)
	if res.CapacitorStable {
		t.Fatal("cap reported stable with draw 10 > peak 6.25")
	}
	if res.CapacitorDepletionSecs < 15 || res.CapacitorDepletionSecs > 300 {
		t.Fatalf("cap depletion = %v s, want a plausible minute-ish value", res.CapacitorDepletionSecs)
	}

	// Mobility: mass 1,190,000 + 375,000 (plate) = 1,565,000;
	// align = mass * agility * ln(4) / 1e6; speed untouched at
	// 355 (no propulsion module); signature 37 + 2 (extender).
	near(t, "mass", res.Mass, 1565000, 1)
	near(t, "align", res.AlignSeconds, 1565000*2.9*math.Log(4)/1e6, 0.001)
	near(t, "velocity", res.Velocity, 355, 1e-9)
	near(t, "signature", res.SignatureRadius, 39, 1e-9)
	near(t, "cargo (physics fallback)", res.CargoCapacity, 275, 1e-9)

	// The charge's otherID effects bound silently; nothing about
	// this ordinary fit should be reported unmodeled.
	for _, note := range res.Unmodeled {
		t.Fatalf("unexpected unmodeled note: %s", note)
	}
}

// TestFitEngineDrones checks drone activation limits and DPS.
func TestFitEngineDrones(t *testing.T) {
	fx := newFitFixture()
	// Drone boat: 25 Mbit/s bandwidth, 500 m3 bay.
	fx.attrs(1002, map[int64]float64{1271: 25, 283: 500, 263: 100, 265: 100, 9: 100, 482: 100, 55: 100000, 479: 100000})
	// Hobgoblin II (2456) real values: thermal 20 per volley,
	// damageMultiplier 1.92, 4 s cycle, 5 Mbit/s each.
	fx.attrs(2456, map[int64]float64{118: 20, 64: 1.92, 51: 4000, 1272: 5})
	fx.group(2456, 100)
	fx.phys(2456, fitPhysics{Mass: 3000, Volume: 5})
	fx.meta(64, false, 1)

	fit := []fitItemInput{{TypeID: 2456, Quantity: 5}}

	// Drones V: 5 active, DPS = 5 * 20 * 1.92 / 4 = 48.
	res := computeFit(fx.snap, 1002, fit, map[int64]int{3436: 5}, nil)
	if res.DronesActive != 5 {
		t.Fatalf("drones active = %d, want 5", res.DronesActive)
	}
	near(t, "drone DPS (V)", res.DroneDPS, 48, 0.001)
	near(t, "bandwidth used", res.DroneBandwidthUsed, 25, 1e-9)
	near(t, "drone bay used", res.DroneBayUsed, 25, 1e-9)

	// Drones II: only 2 controlled.
	res = computeFit(fx.snap, 1002, fit, map[int64]int{3436: 2}, nil)
	if res.DronesActive != 2 {
		t.Fatalf("drones active = %d, want 2", res.DronesActive)
	}
	near(t, "drone DPS (II)", res.DroneDPS, 19.2, 0.001)
}

// TestFitEngineCapacitorEquilibrium pins the pyfa-style cap
// state: the equilibrium percentage when stable.
func TestFitEngineCapacitorEquilibrium(t *testing.T) {
	fx := newFitFixture()
	fx.attrs(1003, map[int64]float64{482: 400, 55: 160000, 263: 100, 265: 100, 9: 100, 479: 100000})
	// Draw 5 GJ/s (5 GJ per 1 s cycle): below the 6.25 peak.
	fx.attrs(2013, map[int64]float64{6: 5, 73: 1000})
	res := computeFit(fx.snap, 1003, []fitItemInput{{TypeID: 2013}}, nil, nil)
	if !res.CapacitorStable {
		t.Fatal("cap should be stable at draw 5 < peak 6.25")
	}
	// Solve (sqrt(x)-x) = 5*160/(10*400) = 0.2 for the upper
	// root: x = ((1+sqrt(1-0.8))/2)^2 = 0.5236 -> 52.36%.
	s := (1 + math.Sqrt(1-0.8)) / 2
	near(t, "cap stable %", res.CapacitorStablePercent, 100*s*s, 0.05)

	// No draw at all: stable at 100%.
	res = computeFit(fx.snap, 1003, nil, nil, nil)
	if !res.CapacitorStable || res.CapacitorStablePercent != 100 {
		t.Fatalf("idle cap = stable %v at %v%%, want stable at 100", res.CapacitorStable, res.CapacitorStablePercent)
	}
}

// TestFitEngineLoadSnapshot drives the DB loader end to end on
// fixture rows in the schema-029 tables.
func TestFitEngineLoadSnapshot(t *testing.T) {
	_, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (597, 'Fixture Punisher', 25, 0, 1),
		   (20349, 'Fixture Plate', 329, 0, 1),
		   (2889, 'Fixture Autocannon', 55, 0, 1)`,
		`INSERT INTO sde_type_physics (type_id, mass, volume, capacity) VALUES (597, 1190000, 28600, 275)`,
		`INSERT INTO sde_attribute_types (attribute_id, name, stackable, high_is_good, unit_id, default_value) VALUES
		   (64, 'damageMultiplier', 0, 1, 0, 1),
		   (265, 'armorHP', 1, 1, 0, 0)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		   (597, 265, 500), (597, 263, 350),
		   (20349, 1159, 1200),
		   (2889, 64, 3.465)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES (2837, 'armorHPBonusAdd', 4)`,
		`INSERT INTO sde_effect_modifiers (effect_id, domain, func, modified_attr, modifying_attr, operation, group_id, skill_type_id)
		   VALUES (2837, 'shipID', 'ItemModifier', 265, 1159, 2, 0, 0)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES (20349, 2837, 0)`,
		`INSERT INTO sde_requirements (type_id, skill_type_id, level) VALUES (2889, 3302, 3)`,
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (3302, 'Fixture Small Projectile Turret', 255, 0, 1)`,
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES (3302, 292, 5)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	snap, err := loadFitSnapshot(ctx, q, []int64{597, 20349, 2889})
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	if got := snap.attrs[597][265]; got != 500 {
		t.Fatalf("snapshot ship armor = %v, want 500", got)
	}
	if got := snap.physics[597].Mass; got != 1190000 {
		t.Fatalf("snapshot ship mass = %v, want 1190000", got)
	}
	// The skill closure pulled 3302 in via the requirement.
	if got := snap.attrs[3302][292]; got != 5 {
		t.Fatalf("snapshot skill attr = %v, want 5", got)
	}
	if eff := snap.effects[2837]; eff == nil || len(eff.Modifiers) != 1 {
		t.Fatalf("snapshot effect 2837 = %+v", eff)
	}

	res := computeFit(snap, 597, []fitItemInput{{TypeID: 20349}}, nil, nil)
	near(t, "armor after plate", res.ArmorHP, 1700, 0.001)
}

// TestFitEngineT3Subsystems pins strategic-cruiser slot morphing:
// the T3 hull carries no slots of its own; fitted subsystems grant
// them through the hiSlotModifier / medSlotModifier /
// lowSlotModifier attributes (1374/1375/1376) plus hardpoint
// modifiers (1368/1369). The dump's slotModifier effect (3774)
// carries no dogma modifiers, so the grants sum directly.
// Fixture values mirror the live dump for a Tengu (29984) with
// four real subsystems (45601/45589/45613/45625): 8/6/2 in game.
func TestFitEngineT3Subsystems(t *testing.T) {
	_, conn, q := buildCorpTestApp(t, &countingTransport{})
	ctx := context.Background()

	stmts := []string{
		`INSERT INTO sde_types (type_id, name, group_id, market_group_id, published) VALUES
		   (29984, 'Fixture Tengu', 963, 0, 1),
		   (45601, 'Fixture Offensive', 956, 0, 1),
		   (45589, 'Fixture Defensive', 954, 0, 1),
		   (45613, 'Fixture Propulsion', 957, 0, 1),
		   (45625, 'Fixture Core', 958, 0, 1)`,
		// Hull: no slots of its own, 3 rig slots.
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		   (29984, 14, 0), (29984, 13, 0), (29984, 12, 0), (29984, 1137, 3)`,
		// Subsystem slot/hardpoint grants (live dump values).
		`INSERT INTO sde_type_attributes (type_id, attribute_id, value) VALUES
		   (45601, 1374, 7), (45601, 1375, 0), (45601, 1376, 0), (45601, 1369, 6),
		   (45589, 1374, 1), (45589, 1375, 3), (45589, 1376, 0),
		   (45613, 1374, 0), (45613, 1375, 0), (45613, 1376, 1),
		   (45625, 1374, 0), (45625, 1375, 3), (45625, 1376, 1)`,
		`INSERT INTO sde_effects (effect_id, name, category) VALUES (3772, 'subSystem', 0)`,
		`INSERT INTO sde_type_effects (type_id, effect_id, is_default) VALUES
		   (45601, 3772, 0), (45589, 3772, 0), (45613, 3772, 0), (45625, 3772, 0)`,
	}
	for _, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	snap, err := loadFitSnapshot(ctx, q, []int64{29984, 45601, 45589, 45613, 45625})
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}

	// Bare hull: no slots.
	bare := computeFit(snap, 29984, nil, nil, nil)
	if bare.HighSlots != 0 || bare.MediumSlots != 0 || bare.LowSlots != 0 {
		t.Fatalf("bare T3 slots = %d/%d/%d, want 0/0/0",
			bare.HighSlots, bare.MediumSlots, bare.LowSlots)
	}

	// Full subsystem set: 8 high, 6 mid, 2 low, 6 launcher hardpoints.
	res := computeFit(snap, 29984, []fitItemInput{
		{TypeID: 45601}, {TypeID: 45589}, {TypeID: 45613}, {TypeID: 45625},
	}, nil, nil)
	if res.HighSlots != 8 || res.MediumSlots != 6 || res.LowSlots != 2 {
		t.Fatalf("T3 slots = %d/%d/%d, want 8/6/2",
			res.HighSlots, res.MediumSlots, res.LowSlots)
	}
	if res.LauncherHardpoints != 6 {
		t.Fatalf("T3 launcher hardpoints = %d, want 6", res.LauncherHardpoints)
	}
	if res.RigSlots != 3 {
		t.Fatalf("T3 rig slots = %d, want 3", res.RigSlots)
	}
}

// TestFitEngineSubsystemSkillScaling pins the Loki acceptance
// case with the live SDE's per-level pattern (verified
// 2026-10-05): the subsystem skill's effect pre-multiplies the
// subsystem's bonus attribute by skillLevel (op 0), and the
// subsystem's effect applies the scaled bonus to webifiers
// (op 6, postPercent). Effective strength = base x (1 +
// bonusPerLevel x skillLevel).
func TestFitEngineSubsystemSkillScaling(t *testing.T) {
	const (
		ship   = 1002
		sub    = 6001
		web    = 6002
		skill  = 2002
		subGrp = 900
		webGrp = 902
		bonus  = 2000 // web bonus attr, 10.0 = 10% per level
	)
	f := newFitFixture().
		group(ship, 25).group(sub, subGrp).group(web, webGrp).
		attrs(ship, map[int64]float64{14: 3, 13: 3, 12: 2}).
		attrs(sub, map[int64]float64{bonus: 10.0}).
		attrs(web, map[int64]float64{20: -60.0}).
		attrs(skill, map[int64]float64{}).
		flag(sub, 3772). // subsystem slot marker
		flag(web, 12).   // high slot marker
		effect(sub, 9001, 0,
			fitModGroup(20, bonus, 6, webGrp)).
		effect(skill, 9002, 0,
			fitModGroup(bonus, 280, 0, subGrp)).
		requires(sub, fitSkillReq{SkillTypeID: skill, Level: 1})
	items := []fitItemInput{{TypeID: sub, Quantity: 1}, {TypeID: web, Quantity: 1}}

	// Level 3: 10% x 3 -> factor 1.3 -> -60 x 1.3 = -78.
	res := computeFit(f.snap, ship, items, map[int64]int{skill: 3}, nil)
	near(t, "web strength at skill 3", res.ItemAttrs[web][20], -78, 1e-9)

	// All V: factor 1.5 -> -90.
	res = computeFit(f.snap, ship, items, map[int64]int{skill: 5}, nil)
	near(t, "web strength at skill 5", res.ItemAttrs[web][20], -90, 1e-9)
}

// TestFitEngineDPSVolley pins DPS and volley on a gunned ship:
// 5x T2 medium guns (damageMultiplier 5, 5s cycle, charge volley
// 20) plus drones. Volley = multiplier x charge volley; DPS =
// volley / cycle.
func TestFitEngineDPSVolley(t *testing.T) {
	const (
		ship   = 1001
		gun    = 3001
		charge = 4001
		drone  = 5001
	)
	f := newFitFixture().
		group(ship, 25).group(gun, 59).group(charge, 901).group(drone, 100).
		attrs(ship, map[int64]float64{14: 3, 13: 3, 12: 2, 1271: 50, 283: 50}).
		attrs(gun, map[int64]float64{64: 5, 51: 5000, 50: 15, 30: 10}).
		attrs(charge, map[int64]float64{114: 5, 116: 5, 117: 5, 118: 5, 64: 1}).
		attrs(drone, map[int64]float64{1272: 10, 51: 1000, 64: 1.2, 114: 4, 116: 3, 117: 2, 118: 1}).
		flag(gun, 12). // high slot
		flag(gun, 42). // turret fitted (effect 42)
		phys(drone, fitPhysics{Volume: 5})
	// effect 42 = turret marker; the engine checks fitEffectTurretFitted.
	f.snap.typeEffects[gun] = append(f.snap.typeEffects[gun], 42)
	f.snap.effects[42] = &fitEffect{Category: 0}

	items := []fitItemInput{{TypeID: gun, Quantity: 5}, {TypeID: drone, Quantity: 5}}
	charges := map[int64]int64{gun: charge}
	res := computeFit(f.snap, ship, items, map[int64]int{3436: 5}, charges)

	// Per gun: volley 20 x multiplier 5 = 100; 5 guns = 500.
	near(t, "turret volley", res.TurretVolley, 500, 1e-9)
	// DPS: 500 / 5s = 100.
	near(t, "turret DPS", res.TurretDPS, 100, 1e-9)
	// Drones: 5 active (bandwidth 50/10), volley 10 x 1.2 = 12 each.
	near(t, "drone volley", res.DroneVolley, 60, 1e-9)
	near(t, "drone DPS", res.DroneDPS, 60, 1e-9)
	near(t, "total DPS", res.DPS, 160, 1e-9)
	near(t, "total volley", res.Volley, 560, 1e-9)
}

// TestFitEngineCapWarfare pins neutralizer and nosferatu drain.
// Remote capacitor transmitters (group 67) are logistics, not
// offensive drain, even though they share powerTransferAmount.
func TestFitEngineCapWarfare(t *testing.T) {
	const (
		ship = 1001
		neut = 6003
		nos  = 6004
		xfer = 6005
	)
	f := newFitFixture().
		group(ship, 25).group(neut, 903).group(nos, 68).group(xfer, 67).
		attrs(ship, map[int64]float64{14: 3, 13: 3, 12: 2}).
		attrs(neut, map[int64]float64{97: 180, 73: 12000}).
		attrs(nos, map[int64]float64{90: 90, 73: 10000}).
		attrs(xfer, map[int64]float64{90: 90, 73: 10000}).
		flag(neut, 13).flag(nos, 13).flag(xfer, 13)
	items := []fitItemInput{
		{TypeID: neut, Quantity: 2},
		{TypeID: nos, Quantity: 1},
		{TypeID: xfer, Quantity: 1},
	}
	res := computeFit(f.snap, ship, items, map[int64]int{2001: 5}, nil)

	near(t, "neut per cycle", res.NeutDrainPerCycle, 360, 1e-9)
	near(t, "neut per sec", res.NeutDrainPerSec, 30, 1e-9)
	near(t, "nos per cycle", res.NosDrainPerCycle, 90, 1e-9)
	near(t, "nos per sec", res.NosDrainPerSec, 9, 1e-9)
}

// TestFitEngineShipRestricted: a siege module (canFitShipGroup01
// = 485 Dreadnought) is allowed on a dread and flagged on a
// cruiser.
func TestFitEngineShipRestricted(t *testing.T) {
	const (
		dread = 1003
		ship  = 1001
		siege = 6006
	)
	f := newFitFixture().
		group(dread, 485).group(ship, 25).group(siege, 903).
		attrs(dread, map[int64]float64{14: 3, 13: 3, 12: 2}).
		attrs(ship, map[int64]float64{14: 3, 13: 3, 12: 2}).
		attrs(siege, map[int64]float64{1298: 485}).
		flag(siege, 13)
	items := []fitItemInput{{TypeID: siege, Quantity: 1}}

	res := computeFit(f.snap, dread, items, map[int64]int{2001: 5}, nil)
	if len(res.Restricted) != 0 {
		t.Errorf("siege on dread: %d restrictions, want 0", len(res.Restricted))
	}

	res = computeFit(f.snap, ship, items, map[int64]int{2001: 5}, nil)
	if len(res.Restricted) != 1 {
		t.Fatalf("siege on cruiser: %d restrictions, want 1", len(res.Restricted))
	}
	if res.Restricted[0].TypeID != siege {
		t.Errorf("restricted type = %d, want %d", res.Restricted[0].TypeID, siege)
	}
	if len(res.Restricted[0].NeedGroup) != 1 || res.Restricted[0].NeedGroup[0] != 485 {
		t.Errorf("restricted needs %v, want group [485]", res.Restricted[0].NeedGroup)
	}
}
