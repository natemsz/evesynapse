package fit

import "math"

// Effects that mark what a module or drone does for others or to
// the world rather than to its own ship: remote repair and capacitor
// transfer, mining, bombs and doomsdays. The SDE ships them without
// modifier rows; the game applies them in code.
const (
	effectRemoteCap              = 6184
	effectRemoteHull             = 6185
	effectRemoteShield           = 6186
	effectRemoteArmor            = 6188
	effectAncillaryRemoteArmor   = 6651
	effectAncillaryRemoteShield  = 6652
	effectMutadaptiveRemoteArmor = 7166
	effectDroneRemoteArmor       = 6687
	effectDroneRemoteShield      = 6688
	effectDroneRemoteHull        = 6689

	effectMiningLaser  = 67
	effectDroneMining  = 17
	effectMiningClouds = 2726

	effectBombLaunching = 2971

	effectEmergencyHullEnergizer = 6484
	effectIndustrialInvulnerable = 6719
	effectWarpDisruptSphere      = 3380
)

// doomsdayEffects are the titans' and dreadnoughts' superweapons:
// one target, a lance or reaper sweeping an area, or a cone.
var doomsdayEffects = []int64{4489, 4490, 4491, 4492, 6201, 6472, 6473}

const (
	attrMiningAmount          = 77
	attrMiningWasteChance     = 3154
	attrMiningCritChance      = 5967
	attrMiningCritBonusYield  = 5969
	attrRemoteSpoolPerCycle   = 2796
	attrRemoteSpoolMax        = 2797
	attrExplosionRange        = 107
	attrDoomsdayDamageRadius  = 2263
	attrDoomsdayDamageSeconds = 2264
	attrDoomsdayDamageCycle   = 2265
	attrMassBonusPercentage   = 1131
	attrMaxVelocityMultiplier = 1470

	skillIceHarvesting          = 16281
	skillIceHarvestingDroneOper = 43702
)

// remoteKind is what a module or drone repairs on someone else:
// "shield", "armor", "hull", "cap", or "" when it does none of that.
func remoteKind(snap *Snapshot, typeID int64) string {
	for _, id := range snap.TypeEffects[typeID] {
		switch id {
		case effectRemoteShield, effectAncillaryRemoteShield, effectDroneRemoteShield:
			return "shield"
		case effectRemoteArmor, effectAncillaryRemoteArmor, effectMutadaptiveRemoteArmor, effectDroneRemoteArmor:
			return "armor"
		case effectRemoteHull, effectDroneRemoteHull:
			return "hull"
		case effectRemoteCap:
			return "cap"
		}
	}
	return ""
}

// miningKind is what a module or drone harvests: "ore", "ice",
// "gas", or "" when it mines nothing.
func miningKind(snap *Snapshot, typeID int64) string {
	switch {
	case snap.HasEffect(typeID, effectMiningClouds):
		return "gas"
	case snap.HasEffect(typeID, effectMiningLaser), snap.HasEffect(typeID, effectDroneMining):
		if snap.requiresSkill(typeID, skillIceHarvesting) || snap.requiresSkill(typeID, skillIceHarvestingDroneOper) {
			return "ice"
		}
		return "ore"
	}
	return ""
}

// support totals what the fit does for others and to the world:
// remote repair and capacitor transfer, mining, bombs and
// doomsdays, from cycling modules and launched drones.
func (f *fitting) support() {
	snap, res := f.snap, f.res
	for _, ent := range f.fitted {
		if ent.kind != entModule || !ent.cycling() {
			continue
		}
		cycle := cycleSeconds(ent, snap)
		if cycle <= 0 {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		if snap.HasEffect(ent.typeID, effectIndustrialInvulnerable) {
			// Invulnerable while it runs: no hit point figure says so.
			f.noteFrom(ent, "effect 6719 industrial invulnerability: not shown in hit points")
		}
		f.remoteRepair(ent, n, cycle)
		f.mine(ent, n, cycle)
		f.bombsAndDoomsdays(ent, cycle)
	}
	for _, d := range f.droneFlying {
		if cycle := cycleSeconds(d.ent, snap); cycle > 0 {
			f.remoteRepair(d.ent, float64(d.n), cycle)
			f.mine(d.ent, float64(d.n), cycle)
		}
	}
	if res.RemoteArmorSpooled > 0 {
		res.RemoteArmorSpooled += res.RemoteArmorRate
	}

	// The capacitor with nosferatu taking from a target whose
	// capacitor is fuller: the drain comes back as capacitor.
	if res.NosDrainPerSec > 0 && res.CapacitorCapacity > 0 {
		recharge := f.shipAttr(AttrCapacitorRecharge) / 1000
		net := res.CapacitorDraw - res.CapacitorInjection - res.NosDrainPerSec
		switch {
		case net <= 0:
			res.CapacitorNosStable, res.CapacitorNosPercent = true, 100
		case net <= res.CapacitorPeakRecharge:
			res.CapacitorNosStable = true
			res.CapacitorNosPercent = capacitorEquilibriumPercent(res.CapacitorCapacity, recharge, net)
		default:
			res.CapacitorNosDepletionSecs = capacitorDepletionSeconds(res.CapacitorCapacity, recharge, net)
		}
	}
}

// remoteRepair adds a module's or drones' repair or capacitor
// transfer on another ship.
func (f *fitting) remoteRepair(ent *entity, n, cycle float64) {
	snap, res := f.snap, f.res
	switch remoteKind(snap, ent.typeID) {
	case "shield":
		res.RemoteShieldRate += n * ent.get(snap, AttrShieldBonus) / cycle
	case "armor":
		amt := ent.get(snap, AttrArmorDamageAmount)
		if mult := ent.get(snap, AttrChargedArmorDamageMult); mult > 0 && f.weaponCharge[ent.typeID] != nil {
			amt *= mult // paste loaded
		}
		res.RemoteArmorRate += n * amt / cycle
		if maxBonus, step := ent.get(snap, attrRemoteSpoolMax), ent.get(snap, attrRemoteSpoolPerCycle); maxBonus > 0 && step > 0 {
			res.RemoteArmorSpooled += n * amt * maxBonus / cycle
			res.RemoteArmorSpoolSeconds = math.Max(res.RemoteArmorSpoolSeconds, math.Ceil(maxBonus/step-1e-9)*cycle)
			res.Ramps = append(res.Ramps, Ramp{TypeID: ent.typeID, Kind: RampArmor, Count: int(n),
				Amount: amt, Cycle: cycle, Step: step, Max: maxBonus})
		}
	case "hull":
		res.RemoteHullRate += n * ent.get(snap, AttrStructureDamageAmt) / cycle
	case "cap":
		res.RemoteCapRate += n * ent.get(snap, AttrPowerTransferAmount) / cycle
	}
}

// mine adds a module's or drones' harvest, in m3 a second, counting
// the share of cycles that crit for extra yield.
func (f *fitting) mine(ent *entity, n, cycle float64) {
	snap, res := f.snap, f.res
	kind := miningKind(snap, ent.typeID)
	if kind == "" {
		return
	}
	amt := ent.get(snap, attrMiningAmount)
	if chance := ent.get(snap, attrMiningCritChance); chance > 0 {
		amt *= 1 + chance*ent.get(snap, attrMiningCritBonusYield)
	}
	rate := n * amt / cycle
	switch kind {
	case "ore":
		res.MiningOre += rate
	case "ice":
		res.MiningIce += rate
	case "gas":
		res.MiningGas += rate
	}
	if w := ent.get(snap, attrMiningWasteChance); w > 0 {
		res.MiningWastePercent = math.Max(res.MiningWastePercent, w)
	}
}

// bombsAndDoomsdays records a bomb launcher's bomb and a doomsday's
// strike: area or one-off damage on long cycles, so not in DPS.
func (f *fitting) bombsAndDoomsdays(ent *entity, cycle float64) {
	snap, res := f.snap, f.res
	if charge := f.weaponCharge[ent.typeID]; charge != nil && snap.HasEffect(charge.typeID, effectBombLaunching) {
		dmg := 0.0
		for _, a := range damageAttrs {
			dmg += charge.get(snap, a)
		}
		if dmg > 0 {
			res.BombDamage = dmg
			res.BombRadius = charge.get(snap, attrExplosionRange)
			res.BombSeconds = cycle
		}
		return
	}
	for _, id := range doomsdayEffects {
		if !snap.HasEffect(ent.typeID, id) {
			continue
		}
		dmg := 0.0
		for _, a := range damageAttrs {
			dmg += ent.get(snap, a)
		}
		res.DoomsdayDamage = dmg
		res.DoomsdaySeconds = cycle
		// A lance or reaper deals its damage every tick for a while,
		// over an area.
		if tick := ent.get(snap, attrDoomsdayDamageCycle); tick > 0 {
			res.DoomsdayTicks = int(math.Round(ent.get(snap, attrDoomsdayDamageSeconds) / tick))
			res.DoomsdayRadius = ent.get(snap, attrDoomsdayDamageRadius)
		}
		return
	}
}
