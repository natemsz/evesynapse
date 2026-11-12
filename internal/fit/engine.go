// Package fit is the fitting simulator's stat engine: given a ship,
// what is fitted to it, the pilot's skill levels and the charges
// loaded, it works out the fit's statistics. The app's fitting pages
// are its only caller. It knows nothing about pages, sessions or ESI.
package fit

// ---------------------------------------------------------------------------
// Fitting simulator stat engine.
//
// Compute takes a ship, a set of fitted items (modules, rigs,
// drones with quantities), a skill level per skill type, and a
// charge choice per weapon type, loads the dogma data for those
// types from the SDE tables (LoadSnapshot), and computes the
// headline fit statistics: resources, capacitor, DPS by source,
// EHP, repair rates, mobility and targeting.
//
// Nothing here does I/O except the snapshot loader; the math is
// pure over the loaded snapshot, so calibration tests drive it
// with synthetic SDE rows.
//
// ---------------------------------------------------------------------------
// Operation semantics (pinned empirically from the live dump
// 2026-10-04, three-way cross-checked below; see also the
// importer tests):
//
//   -1 PreAssign     apply value := v
//   0  PreMultiply   value *= v
//   1  PreDivide     value /= v
//   2  ModAdd        value += v
//   3  ModSub        value -= v
//   4  PostMultiply  value *= v                (stacking-penalized)
//   5  PostDivide    value /= v                (stacking-penalized)
//   6  PostPercent   value *= (1 + v/100)      (stacking-penalized)
//   7  PostAssign    value := v
//
// Evidence (dgmEffects effectID + shape):
//   - effect 21 shieldCapacityBonusOnline and 2837/3771
//     armorHPBonus*(Add): shipID/ItemModifier, operation 2,
//     ship shieldCapacity/armorHP grows by the module's flat
//     capacityBonus/armorHPBonusAdd attribute -> op 2 is additive.
//   - effect 105 shieldResonanceMultiplyOnline: operation 4
//     multiplies ship resonances by the module's
//     *ResonanceMultiplier attribute -> op 4 is a direct ratio.
//   - effect 958 shipArmorEmResistanceAC2 (on the Maller):
//     operation 6 against shipBonusAC2 = -4 -> at skill level 5
//     the skill's own effect 926 pre-multiplies the bonus to -20
//     and resonance becomes base * (1 + -20/100), e.g. armor EM
//     0.5 * 0.8 = 0.40 = 60% resist, matching the game.
//   - effect 2302 damageControl uses operation 0 with resonance
//     values like 0.85 stored on the module -> pre-multiply.
//   - effect 4240/4247 are literally named
//     modify*ResonancePassivePreAssignment (op -1) and effect
//     6010..6015 are named shipMode*PostDiv (op 5); effect 2886
//     setMaxLockedTargets is op 7 (assignment). Effect 500
//     amarrCruiserSkillLevelPreMulShipBonusACShip is op 0.
//
// The one operation not modeled is 9: only effect 132
// (skillEffect) uses it, to fold skillTimeConstant into
// skillLevel; skill levels are an engine input here, so
// modifiers targeting attribute 280 are skipped by construction.
//
// Stacking penalties apply to the multiplicative operations
// (4, 5, 6) when the *modified* attribute is flagged
// non-stackable in dgmAttributeTypes AND the modifier's source is
// a fitted module/rig/subsystem. Ship hull bonuses and skill
// effects are never penalized. Penalized modifiers
// on one attribute split into bonus (>1) and penalty (<1)
// chains; each chain is ordered by |factor-1| descending and
// its i-th entry (0-based) applies at effectiveness
// exp(-i^2 / 7.1289): 1.0, ~0.869, ~0.571, ~0.283, the game's
// well-known curve. (The design sketch said 0.87^n; that matches
// only the first step of the real curve, so the eos formula is
// implemented and the difference is flagged in the ship notes.)
// eos's own Operator enum (PREASSIGN/PREINCREASE/MULTIPLY/
// POSTINCREASE/FORCE) is a handler-level abstraction above the
// SDE operation codes, not a conflicting mapping; the SDE codes
// above are the source data both engines consume.
//
// Modifier funcs modeled: ItemModifier (shipID/charID/itemID
// domains, plus charge->weapon otherID), LocationModifier,
// LocationGroupModifier (invGroups group match),
// LocationRequiredSkillModifier (sde_requirements match), and
// OwnerRequiredSkillModifier (drones, charges and missiles), plus
// the modifier-less effects in codeEffects. Domains
// targetID / target / structureID, the EffectStopper func, effect
// categories outside {0 passive, 1 active, 4 online}, except 5 =
// overload, which binds for overheated modules, and charge/group
// shapes that target nothing listed here are skipped and reported
// in Result.Unmodeled, never silently dropped. A heatable
// module that simply isn't overheated skips its overload effects
// silently: that's a fitting choice, not a modeling gap.
//
// Skill interplay is data-driven, not special-cased: a trained
// skill becomes a pseudo-item whose attributes start from its
// type row with skillLevel (280) set from the input. Its own
// effects then do what the dump says -- e.g. Large Hybrid
// Turret's effect 152 pre-multiplies its damageMultiplierBonus
// by skillLevel, and effect 157 applies the scaled bonus to
// every fitted item requiring that skill. Skill level 0 (or an
// untrained/absent skill) contributes nothing.
//
// Deliberate v1 scope (per the design doc): all fitted modules
// count as online and cycling; propulsion-module velocity is
// computed with the thrust/mass formula (their effects carry no
// modifiers in the dump); missile figures are raw DPS (no
// application math). T3 strategic-cruiser subsystems grant slots
// and hardpoints through their hiSlotModifier / medSlotModifier /
// lowSlotModifier / hardPointModifier attributes (summed directly;
// the dump's slotModifier effect 3774 carries no dogma modifiers,
// so there is no modifier machinery to run, verified against the
// live dump 2026-10-05); their PG/CPU and role bonuses flow through
// the normal modifier machinery.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
)

// Dogma attribute IDs the engine reads (dgmAttributeTypes names
// verified against the live dump 2026-10-04).
const (
	AttrMass               = 4
	AttrCapacitorNeed      = 6
	AttrHP                 = 9
	AttrPowerOutput        = 11
	AttrLowSlots           = 12
	AttrMedSlots           = 13
	AttrHiSlots            = 14
	AttrSpeedFactor        = 20
	AttrPower              = 30
	AttrMaxVelocity        = 37
	AttrCapacity           = 38
	AttrCPUOutput          = 48
	AttrCPU                = 50
	AttrSpeed              = 51
	AttrMaxRange           = 54
	AttrCapacitorRecharge  = 55
	AttrDamageMultiplier   = 64
	AttrShieldBonus        = 68
	AttrAgility            = 70
	AttrDuration           = 73
	AttrReactivationDelay  = 669
	AttrMaxGroupFitted     = 1544
	AttrMaxTargetRange     = 76
	AttrStructureDamageAmt = 83
	AttrArmorDamageAmount  = 84
	AttrLauncherSlotsLeft  = 101
	AttrTurretSlotsLeft    = 102
	AttrResonanceEM        = 113 // generic (hull) EM resonance
	AttrResonanceExplosive = 111
	AttrResonanceKinetic   = 109
	AttrResonanceThermal   = 110
	AttrDamageEM           = 114
	AttrDamageExplosive    = 116
	AttrDamageKinetic      = 117
	AttrDamageThermal      = 118
	AttrFalloff            = 158
	AttrTrackingSpeed      = 160
	AttrMaxLockedTargets   = 192
	AttrScanRadar          = 208
	AttrScanLadar          = 209
	AttrScanMagnetometric  = 210
	AttrScanGravimetric    = 211
	AttrMissileDamageMult  = 212
	AttrShieldCapacity     = 263
	AttrArmorHP            = 265
	AttrArmorEM            = 267
	AttrArmorExplosive     = 268
	AttrArmorKinetic       = 269
	AttrArmorThermal       = 270
	AttrShieldEM           = 271
	AttrShieldExplosive    = 272
	AttrShieldKinetic      = 273
	AttrShieldThermal      = 274
	AttrSkillLevel         = 280
	AttrDroneCapacity      = 283
	AttrCapacitorCapacity  = 482
	AttrShieldRechargeRate = 479
	AttrSignatureRadius    = 552
	AttrScanResolution     = 564
	AttrSpeedBoostFactor   = 567
	AttrMassAddition       = 796
	AttrUpgradeCapacity    = 1132
	AttrRigSlots           = 1137
	AttrUpgradeCost        = 1153
	AttrDroneBandwidth     = 1271
	AttrDroneBandwidthUsed = 1272
	AttrDronesSkill        = 3436
	// T3 subsystem slot/hardpoint grants (dgmAttributeTypes,
	// verified against the live dump 2026-10-05): subsystems
	// carry these as plain attributes; the dump's slotModifier
	// effect (3774) has no dogma modifiers, so the grants sum
	// directly instead of flowing through the modifier machinery.
	AttrTurretHardPointModifier   = 1368
	AttrLauncherHardPointModifier = 1369
	AttrHiSlotModifier            = 1374
	AttrMedSlotModifier           = 1375
	AttrLowSlotModifier           = 1376
	// Ship-restriction attributes (dgmAttributeTypes, verified
	// against the live dump 2026-10-05): a module carrying any of
	// these may only be fitted when the hull matches one of the
	// named ship groups or types. Siege modules point at group
	// 485 (Dreadnought) via canFitShipGroup01; bastion at
	// marauders, triage at carriers, same shape.
	AttrCanFitShipGroup01 = 1298
	AttrCanFitShipGroup04 = 1301
	AttrCanFitShipType1   = 1302
	AttrCanFitShipType4   = 1305
	// Subsystems name their one hull in fitsToShipType (1380) and
	// their slot in subSystemSlot (1366): one subsystem per slot.
	AttrFitsToShipType = 1380
	AttrSubSystemSlot  = 1366
	// Cap-warfare amounts (dgmAttributeTypes): neutralizers drain
	// via energyNeutralizerAmount (97); nosferatu via
	// powerTransferAmount (90), which remote capacitor
	// transmitters (group 67) share, group 68 is the nosferatu
	// group, so the engine only counts 68 as offensive drain.
	AttrEnergyNeutralizerAmount = 97
	AttrPowerTransferAmount     = 90
	groupRemoteCapTransmitter   = 67
	groupNosferatu              = 68
	groupSmartbomb              = 72
)

// CharacterTypeID is the SDE's Character type: the pilot's base
// attributes, before skills and implants.
const CharacterTypeID = 1373

// Capacitor boosters: the charge's injection, and the module's
// reload between clips.
const (
	AttrCapacitorBonus = 67
	AttrReloadTime     = 1795
)

// Slot / fitting effect IDs (dgmEffects names verified live).
const (
	EffectLoPower = 11
	// EffectOnline is the onlining effect every module carries; the
	// dump files it in the active category (1), so it never makes a
	// module activatable on its own.
	EffectOnline         = 16
	EffectHiPower        = 12
	EffectMedPower       = 13
	EffectLauncherFitted = 40
	EffectTurretFitted   = 42
	EffectRigSlot        = 2663
	EffectSubsystemSlot  = 3772
)

// Effect categories (dgmEffects): 5 is the overload/heat
// category, the one heating a module engages.
// Ancillary armor repairers, spool-up weapons, Breacher pods and the
// Reactive Armor Hardener.
const (
	// AttrChargedArmorDamageMult is an ancillary armor repairer's
	// multiplier while it has Nanite Repair Paste loaded.
	AttrChargedArmorDamageMult = 1886
	// Spool-up weapons: damage bonus gained per cycle and its cap.
	AttrSpoolBonusPerCycle = 2733
	AttrSpoolBonusMax      = 2734
	// Breacher pod damage over time (on the charge).
	AttrDotDuration            = 5735
	AttrDotMaxDamagePerTick    = 5736
	AttrDotMaxHPPercentPerTick = 5737
	// EffectAdaptiveArmorHardener is the Reactive Armor Hardener's
	// adaptation (no modifier rows; see codeEffects).
	EffectAdaptiveArmorHardener = 4928
)

// Fighters: squadron shape, the carrier's tubes and role slots,
// and the attack and missile abilities (the rest of the abilities
// are in fighters.go). Attack is missile-style or turret-style by
// fighter.
const (
	AttrFighterCapacity          = 2055
	AttrFighterSquadronIsLight   = 2212
	AttrFighterSquadronIsSupport = 2213
	AttrFighterSquadronIsHeavy   = 2214
	AttrFighterSquadronMaxSize   = 2215
	// Fighter propulsion abilities (no modifier rows: fitview
	// applies them to the fighter's effective speed and signature).
	AttrFighterABSpeedBonus      = 2151
	AttrFighterMWDSpeedBonus     = 2152
	AttrFighterMWDSigBonus       = 2153
	AttrFighterMJDDistance       = 2154
	AttrFighterMWDDuration       = 2157
	AttrFighterABDuration        = 2158
	AttrFighterEvasiveDuration   = 2123
	AttrFighterEvasiveSpeedBonus = 2224
	AttrFighterEvasiveSigBonus   = 2225
	// The fighter propulsion ability effects themselves.
	EffectFighterEvasive    = 6439
	EffectFighterAB         = 6440
	EffectFighterMWD        = 6441
	EffectFighterMJD        = 6442
	AttrFighterTubes        = 2216
	AttrFighterLightSlots   = 2217
	AttrFighterSupportSlots = 2218
	AttrFighterHeavySlots   = 2219

	AttrFighterAttackMissileMult     = 2226
	AttrFighterAttackMissileDuration = 2233
	AttrFighterAttackTurretMult      = 2178
	AttrFighterAttackTurretDuration  = 2177
	AttrFighterMissilesMult          = 2130
	AttrFighterMissilesDuration      = 2182
)

var (
	fighterAttackMissileDamage = []int64{2227, 2228, 2229, 2230}
	fighterAttackTurretDamage  = []int64{2171, 2172, 2173, 2174}
	fighterMissilesDamage      = []int64{2131, 2132, 2133, 2134}
)

const catOverload = 5

// Module states: the values ItemInput.State carries, and a saved
// fit stores per module:
//   - offline:    fitted but contributes nothing (no CPU/PG, no effects)
//   - online:     powered; passive + online-category effects apply,
//     active effects don't, nothing cycles
//   - active:     cycling; passive + online + active effects apply
//   - overheated: active plus the module's overload effects
const (
	StateOnline     = "online"
	StateActive     = "active"
	StateOffline    = "offline"
	StateOverheated = "overheated"
)

// Dogma operation codes (see the header comment for evidence).
const (
	opPreAssign   = -1
	opPreMultiply = 0
	opPreDivide   = 1
	opModAdd      = 2
	opModSub      = 3
	opPostMult    = 4
	opPostDivide  = 5
	opPostPercent = 6
	opPostAssign  = 7
)

// AttrMeta is one dgmAttributeTypes row reduced to what the
// engine needs: stacking behaviour and the default value an
// attribute carries when no type row sets it.
type AttrMeta struct {
	Stackable bool
	Default   float64
}

// Modifier is one decoded dgmEffects modifier.
type Modifier struct {
	Domain        string
	Func          string
	ModifiedAttr  int64
	ModifyingAttr int64
	Operation     int64
	GroupID       int64
	SkillTypeID   int64
}

// Effect is one modifier-bearing effect.
type Effect struct {
	Name      string
	Category  int64
	Modifiers []Modifier
}

// SkillReq is one (type requires skill at level) row.
type SkillReq struct {
	SkillTypeID int64
	Level       int64
}

// Physics is one sde_type_physics row.
type Physics struct {
	Mass     float64
	Volume   float64
	Capacity float64
}

// Snapshot is the engine's read-only view of the dogma data
// for a closed set of types (ship, items, charges, and every
// skill reachable from their requirements/effect selectors).
type Snapshot struct {
	Attrs        map[int64]map[int64]float64 // type -> attribute -> base value
	Physics      map[int64]Physics
	Groups       map[int64]int64 // type -> invGroups group ID
	Meta         map[int64]AttrMeta
	Effects      map[int64]*Effect // every effect; Modifiers may be empty
	TypeEffects  map[int64][]int64 // type -> effect IDs (sorted)
	Requirements map[int64][]SkillReq
	// Bursts is the command burst data (WithBursts); nil leaves
	// bursts without effect.
	Bursts *BurstData
}

// attrDefault returns the value an attribute starts from when a
// type carries no row for it (dgmAttributeTypes defaultValue,
// with hard floors for the multiplicative identities).
func (snap *Snapshot) attrDefault(attr int64) float64 {
	if m, ok := snap.Meta[attr]; ok {
		return m.Default
	}
	switch attr {
	case AttrDamageMultiplier, AttrMissileDamageMult:
		return 1
	}
	return 0
}

// stackable reports whether modifiers on an attribute dodge the
// stacking-penalty group (dgmAttributeTypes stackable flag; an
// unknown attribute is treated as stackable so penalties never
// apply on missing data).
func (snap *Snapshot) stackable(attr int64) bool {
	if m, ok := snap.Meta[attr]; ok {
		return m.Stackable
	}
	return true
}

// requiresSkill reports whether a type's requirement rows name
// the given skill.
func (snap *Snapshot) requiresSkill(typeID, skillTypeID int64) bool {
	for _, r := range snap.Requirements[typeID] {
		if r.SkillTypeID == skillTypeID {
			return true
		}
	}
	return false
}

// HasEffect reports whether a type carries the given effect.
func (snap *Snapshot) HasEffect(typeID, effectID int64) bool {
	for _, id := range snap.TypeEffects[typeID] {
		if id == effectID {
			return true
		}
	}
	return false
}

// typeEffectCats lists the effect categories a type carries.
func (snap *Snapshot) typeEffectCats(typeID int64) map[int64]bool {
	out := make(map[int64]bool)
	for _, eid := range snap.TypeEffects[typeID] {
		if eff := snap.Effects[eid]; eff != nil {
			out[eff.Category] = true
		}
	}
	return out
}

// moduleActivatable reports whether a module cycles: an
// active-category effect other than the onlining effect every
// module carries, or a turret or launcher fitting.
func (snap *Snapshot) moduleActivatable(typeID int64) bool {
	for _, eid := range snap.TypeEffects[typeID] {
		if eid == EffectOnline {
			continue
		}
		if eff := snap.Effects[eid]; eff != nil && eff.Category == 1 {
			return true
		}
	}
	return snap.HasEffect(typeID, EffectTurretFitted) || snap.HasEffect(typeID, EffectLauncherFitted)
}

// ModuleValidStates lists the states a fitted module may take,
// derived from SDE data, never per-module hardcoding: rigs,
// subsystems, drones and cargo have no states; a module is
// activatable with an active-category effect, or as a weapon
// (turret/launcher fitted markers are passive in SDE, but weapons
// cycle via F1 in-game); overheatable only with an
// overload-category effect on top of that (heat is applied to a
// cycling module).
func ModuleValidStates(snap *Snapshot, typeID int64) []string {
	if snap.HasEffect(typeID, EffectRigSlot) || snap.HasEffect(typeID, EffectSubsystemSlot) {
		return nil
	}
	if snap.Attrs[typeID][AttrDroneBandwidthUsed] > 0 {
		return nil
	}
	cats := snap.typeEffectCats(typeID)
	if len(cats) == 0 {
		return nil
	}
	states := []string{StateOffline, StateOnline}
	if snap.moduleActivatable(typeID) {
		states = append(states, StateActive)
		if cats[catOverload] {
			states = append(states, StateOverheated)
		}
	}
	return states
}

// defaultModuleState is the state a fresh module takes: active
// when it can cycle, online otherwise. This preserves the
// engine's historical behavior (everything online and cycling)
// for fits that predate states.
func defaultModuleState(snap *Snapshot, typeID int64) string {
	if snap.moduleActivatable(typeID) {
		return StateActive
	}
	return StateOnline
}

// NormalizeModuleState resolves "" to the type default and
// demotes states the type cannot take (stale docs, bad input) to
// the default. Non-state kinds get "".
func NormalizeModuleState(snap *Snapshot, typeID int64, state string) string {
	valid := ModuleValidStates(snap, typeID)
	if len(valid) == 0 {
		return ""
	}
	for _, v := range valid {
		if v == state {
			return state
		}
	}
	return defaultModuleState(snap, typeID)
}

// sourceCategories lists the effect categories that bind for
// one entity under its module state: offline
// modules bind nothing; online skips active and overload;
// active adds the active category; overheated adds overload.
func sourceCategories(e *entity) map[int64]bool {
	if e.kind != entModule {
		return map[int64]bool{0: true, 1: true, 4: true}
	}
	switch e.state {
	case StateOffline:
		return nil
	case StateOnline:
		return map[int64]bool{0: true, 4: true}
	case StateOverheated:
		return map[int64]bool{0: true, 1: true, 4: true, catOverload: true}
	default:
		return map[int64]bool{0: true, 1: true, 4: true}
	}
}

// StateAttrKey keys per-(type, state) attribute maps.
func StateAttrKey(typeID int64, state string) string {
	return strconv.FormatInt(typeID, 10) + "\x00" + state
}

// LoadSnapshot loads the dogma rows for the given types and
// closes over the skills they touch: requirements (recursive)
// and skill selectors in any carried effect's modifiers. The
// extra rounds discover skill types so their own attributes and
// effects are present when the engine runs.
func LoadSnapshot(ctx context.Context, q *db.Queries, typeIDs []int64) (*Snapshot, error) {
	snap := &Snapshot{
		Attrs:        make(map[int64]map[int64]float64),
		Physics:      make(map[int64]Physics),
		Groups:       make(map[int64]int64),
		Meta:         make(map[int64]AttrMeta),
		Effects:      make(map[int64]*Effect),
		TypeEffects:  make(map[int64][]int64),
		Requirements: make(map[int64][]SkillReq),
	}

	loadedTypes := make(map[int64]bool)
	loadedEffects := make(map[int64]bool)
	// A copy: the caller keeps its slice in the order it built it.
	// The character type rides along: its attributes are the
	// pilot's own base values (two locked targets before skills).
	pending := dedupeSortedIDs(append(append([]int64(nil), typeIDs...), CharacterTypeID))

	for round := 0; round < 6; round++ {
		var fresh []int64
		for _, id := range pending {
			if !loadedTypes[id] {
				fresh = append(fresh, id)
				loadedTypes[id] = true
			}
		}
		if len(fresh) == 0 {
			break
		}

		attrRows, err := q.ListSDETypeAttributesByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type attributes: %w", err)
		}
		for _, r := range attrRows {
			m := snap.Attrs[r.TypeID]
			if m == nil {
				m = make(map[int64]float64)
				snap.Attrs[r.TypeID] = m
			}
			m[r.AttributeID] = r.Value
		}
		physRows, err := q.ListSDETypePhysicsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type physics: %w", err)
		}
		for _, r := range physRows {
			snap.Physics[r.TypeID] = Physics{Mass: r.Mass, Volume: r.Volume, Capacity: r.Capacity}
		}
		groupRows, err := q.ListSDETypeGroupsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type groups: %w", err)
		}
		for _, r := range groupRows {
			snap.Groups[r.TypeID] = r.GroupID
		}
		effectLinks, err := q.ListSDETypeEffectsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type effects: %w", err)
		}
		for _, r := range effectLinks {
			snap.TypeEffects[r.TypeID] = append(snap.TypeEffects[r.TypeID], r.EffectID)
		}
		reqRows, err := q.ListSDERequirementsByTypes(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load requirements: %w", err)
		}
		for _, r := range reqRows {
			snap.Requirements[r.TypeID] = append(snap.Requirements[r.TypeID],
				SkillReq{SkillTypeID: r.SkillTypeID, Level: r.Level})
		}

		// Effects carried by the fresh types (the importer
		// stores every effect; many carry no modifiers).
		var newEffects []int64
		for _, id := range fresh {
			for _, eid := range snap.TypeEffects[id] {
				if !loadedEffects[eid] {
					loadedEffects[eid] = true
					newEffects = append(newEffects, eid)
				}
			}
		}
		if len(newEffects) > 0 {
			newEffects = dedupeSortedIDs(newEffects)
			effectRows, err := q.ListSDEEffectsByIDs(ctx, newEffects)
			if err != nil {
				return nil, fmt.Errorf("load effects: %w", err)
			}
			for _, r := range effectRows {
				snap.Effects[r.EffectID] = &Effect{Name: r.Name, Category: r.Category}
			}
			modRows, err := q.ListSDEEffectModifiersByIDs(ctx, newEffects)
			if err != nil {
				return nil, fmt.Errorf("load effect modifiers: %w", err)
			}
			for _, r := range modRows {
				eff := snap.Effects[r.EffectID]
				if eff == nil {
					eff = &Effect{}
					snap.Effects[r.EffectID] = eff
				}
				eff.Modifiers = append(eff.Modifiers, Modifier{
					Domain:        r.Domain,
					Func:          r.Func,
					ModifiedAttr:  r.ModifiedAttr,
					ModifyingAttr: r.ModifyingAttr,
					Operation:     r.Operation,
					GroupID:       r.GroupID,
					SkillTypeID:   r.SkillTypeID,
				})
			}
		}

		// Attribute metadata for everything seen so far.
		var attrIDs []int64
		for _, id := range fresh {
			for attr := range snap.Attrs[id] {
				attrIDs = append(attrIDs, attr)
			}
		}
		for _, eff := range snap.Effects {
			for _, m := range eff.Modifiers {
				attrIDs = append(attrIDs, m.ModifiedAttr, m.ModifyingAttr)
			}
		}
		attrIDs = dedupeSortedIDs(attrIDs)
		if len(attrIDs) > 0 {
			metaRows, err := q.ListSDEAttributeTypesByIDs(ctx, attrIDs)
			if err != nil {
				return nil, fmt.Errorf("load attribute types: %w", err)
			}
			for _, r := range metaRows {
				snap.Meta[r.AttributeID] = AttrMeta{
					Stackable: r.Stackable != 0,
					Default:   r.DefaultValue,
				}
			}
		}

		// Next round: skills referenced by requirements or by
		// modifier selectors anywhere in the loaded effects.
		var next []int64
		for _, id := range fresh {
			for _, r := range snap.Requirements[id] {
				if !loadedTypes[r.SkillTypeID] {
					next = append(next, r.SkillTypeID)
				}
			}
			// A heavy fighter's bomb is a type of its own: its
			// damage and blast size live on the bomb.
			if bomb := int64(snap.Attrs[id][AttrFighterBombType]); bomb > 0 && !loadedTypes[bomb] {
				next = append(next, bomb)
			}
		}
		for _, eff := range snap.Effects {
			for _, m := range eff.Modifiers {
				if m.SkillTypeID != 0 && !loadedTypes[m.SkillTypeID] {
					next = append(next, m.SkillTypeID)
				}
			}
		}
		pending = dedupeSortedIDs(next)
	}

	for _, ids := range snap.TypeEffects {
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return snap, nil
}

// dedupeSortedIDs normalizes an ID list for sqlc slice queries.
func dedupeSortedIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllVSkillLevels returns the "All V" skill map for a fit:
// every skill reachable from the ship's and items' requirements
// (recursively) and from effect skill selectors, at level 5.
func AllVSkillLevels(snap *Snapshot, shipTypeID int64, itemTypeIDs []int64) map[int64]int {
	skills := make(map[int64]bool)
	queue := append([]int64{shipTypeID}, itemTypeIDs...)
	for len(queue) > 0 {
		typeID := queue[0]
		queue = queue[1:]
		for _, r := range snap.Requirements[typeID] {
			if !skills[r.SkillTypeID] {
				skills[r.SkillTypeID] = true
				queue = append(queue, r.SkillTypeID)
			}
		}
		for _, eid := range snap.TypeEffects[typeID] {
			if eff := snap.Effects[eid]; eff != nil {
				for _, m := range eff.Modifiers {
					if m.SkillTypeID != 0 && !skills[m.SkillTypeID] {
						skills[m.SkillTypeID] = true
						queue = append(queue, m.SkillTypeID)
					}
				}
			}
		}
	}
	levels := make(map[int64]int, len(skills))
	for id := range skills {
		levels[id] = 5
	}
	return levels
}

// ---------------------------------------------------------------------------
// Engine.
// ---------------------------------------------------------------------------

// ItemInput is one fitted item line: a type, how many of
// it the fit carries (drones especially), and, for modules,
// the module state ("", online/active/offline/overheated; ""
// resolves to the type's default).
type ItemInput struct {
	TypeID   int64
	Quantity int
	State    string
	// Abilities is a fighter line's switched-on abilities, the
	// whole set (FighterAbilitiesOn resolves one); nil is the
	// fighter type's defaults. Ignored for anything else.
	Abilities []string
}

// EHPProfile is effective HP against a damage profile.
type EHPProfile struct {
	Omni      float64
	EM        float64
	Thermal   float64
	Kinetic   float64
	Explosive float64
}

// Result is the computed fit. Floats are raw dogma units
// (times in seconds, capacitor in GJ); counts are integers.
type Result struct {
	ShipTypeID int64

	PowergridUsed float64
	PowergridMax  float64
	CPUUsed       float64
	CPUMax        float64

	CalibrationUsed float64
	CalibrationMax  float64

	TurretHardpointsUsed   int
	TurretHardpoints       int
	LauncherHardpointsUsed int
	LauncherHardpoints     int

	HighSlotsUsed   int
	HighSlots       int
	MediumSlotsUsed int
	MediumSlots     int
	LowSlotsUsed    int
	LowSlots        int
	RigSlotsUsed    int
	RigSlots        int

	DronesFitted       int
	DronesActive       int
	DroneBandwidthUsed float64
	DroneBandwidth     float64
	DroneBayUsed       float64 // m3 of fitted drones
	DroneBayCapacity   float64 // m3

	CapacitorCapacity     float64
	CapacitorPeakRecharge float64 // GJ/s at the curve's peak
	CapacitorDraw         float64 // GJ/s with everything cycling
	// CapacitorInjection is what fitted capacitor boosters put back,
	// GJ/s sustained over their reloads. Stability reads the draw
	// less this.
	CapacitorInjection     float64
	CapacitorStable        bool
	CapacitorStablePercent float64 // equilibrium % when stable
	CapacitorDepletionSecs float64 // approx seconds to empty; 0 when stable

	TurretDPS    float64
	MissileDPS   float64
	DroneDPS     float64
	SmartbombDPS float64
	FighterDPS   float64 // launched squadrons' switched-on damage abilities
	DPS          float64
	// SpooledDPS is DPS with every spool-up weapon (entropic
	// disintegrators) at its full damage bonus, reached after
	// SpoolSeconds of continuous fire; both 0 without one.
	SpooledDPS   float64
	SpoolSeconds float64
	// Breacher pods: up to BreacherMaxPerSec HP a second, or
	// BreacherHPPercent of the target's hit points a second when
	// that is lower, for BreacherSeconds per pod. Not in DPS.
	BreacherMaxPerSec float64
	BreacherHPPercent float64
	BreacherSeconds   float64

	// Volley damage per weapon cycle, broken down like DPS.
	TurretVolley    float64
	MissileVolley   float64
	DroneVolley     float64
	SmartbombVolley float64
	FighterVolley   float64
	Volley          float64

	// Fighters: what the hangar carries, what launches (one
	// squadron per tube, within the hull's light, support and heavy
	// slots, the standup slots on a structure), and the launched
	// squadrons' missile salvo. FighterDPS and FighterVolley count
	// every switched-on damage ability: attack, missiles and bombs
	// (missiles and bombs at their rate while charges last).
	// FighterLines is the launch report per fighter line. Every
	// figure is 0 on a hull without fighter tubes.
	FightersCarried       int
	SquadronsLaunched     int
	FighterTubes          int
	FighterLightUsed      int
	FighterLightSlots     int
	FighterSupportUsed    int
	FighterSupportSlots   int
	FighterHeavyUsed      int
	FighterHeavySlots     int
	FighterHangarUsed     float64 // m3
	FighterHangarCapacity float64 // m3
	FighterSalvo          float64 // damage of one salvo from every launched squadron
	FighterSalvoSeconds   float64 // the longest salvo cycle among them
	FighterStandup        bool    // the slots are a structure's standup slots
	FighterLines          []FighterLine

	// Capacitor warfare: neutralizer and nosferatu drain, per
	// cycle and per second, from fitted modules.
	NeutDrainPerCycle float64
	NeutDrainPerSec   float64
	NosDrainPerCycle  float64
	NosDrainPerSec    float64
	// The capacitor with nosferatu running against a target whose
	// capacitor is fuller, so the drain comes back as capacitor.
	// Set only with nosferatu fitted.
	CapacitorNosStable        bool
	CapacitorNosPercent       float64
	CapacitorNosDepletionSecs float64

	// Logistics: what the fit repairs or transfers on another ship,
	// from remote modules and repair drones, a second. A
	// mutadaptive remote repairer ramps up: RemoteArmorSpooled is
	// the armor rate at its full bonus, reached after
	// RemoteArmorSpoolSeconds (both 0 without one).
	RemoteShieldRate        float64
	RemoteArmorRate         float64
	RemoteArmorSpooled      float64
	RemoteArmorSpoolSeconds float64
	RemoteHullRate          float64
	RemoteCapRate           float64

	// Mining: m3 a second of ore, ice and gas, crits counted, and
	// the highest chance a cycle wastes part of its yield (%).
	MiningOre          float64
	MiningIce          float64
	MiningGas          float64
	MiningWastePercent float64

	// A bomb launcher's bomb (damage, blast radius m, seconds
	// between bombs) and a doomsday's strike (damage per hit,
	// seconds between uses; a lance or reaper hits DoomsdayTicks
	// times, a second apart, in DoomsdayRadius m). Not in DPS.
	BombDamage      float64
	BombRadius      float64
	BombSeconds     float64
	DoomsdayDamage  float64
	DoomsdaySeconds float64
	DoomsdayTicks   int
	DoomsdayRadius  float64

	ShieldHP float64
	ArmorHP  float64
	HullHP   float64

	ShieldEHP EHPProfile
	ArmorEHP  EHPProfile
	HullEHP   EHPProfile
	EHP       EHPProfile

	ShieldRegenPeak float64 // HP/s at the recharge curve's peak
	ShieldBoostRate float64 // HP/s from fitted shield boosters
	ArmorRepairRate float64 // HP/s from fitted armor repairers
	HullRepairRate  float64 // HP/s from fitted hull repairers

	Velocity        float64 // m/s, with a fitted propulsion module running
	Mass            float64 // kg, including plate mass additions
	AlignSeconds    float64
	SignatureRadius float64

	MaxTargetRange   float64
	ScanResolution   float64
	SensorStrength   float64 // strongest of the four sensor types
	MaxLockedTargets float64

	CargoCapacity float64

	// ShipAttrs holds the ship's computed attributes (post
	// effects); ItemAttrs the same per fitted type. Charge and
	// missile types appear here too. For the stats UI later.
	ShipAttrs map[int64]float64
	ItemAttrs map[int64]map[int64]float64
	// StateAttrs holds per-(type, state) attributes for module
	// entities whose state isn't the default, keyed by
	// StateAttrKey, so a tooltip can show an overheated
	// module's heated numbers rather than its resting ones.
	StateAttrs map[string]map[int64]float64
	// WeaponDPS and WeaponVolley hold one fitted weapon's own
	// damage figures (a single module instance, not the rack
	// total), keyed by StateAttrKey, the module tooltip reads
	// its own gun's numbers from here. Recorded by offense()
	// alongside the totals so the tooltip can never drift
	// from the math the stats panel shows.
	WeaponDPS    map[string]float64
	WeaponVolley map[string]float64

	// Buffs are the command burst buffs acting on the fit, the fit's
	// own and the fleet's, by buff id.
	Buffs []AppliedBuff

	// Weapons is each fitted weapon group's damage with what its
	// reach and application depend on, in fitting order: the
	// damage-at-range chart's input (WeaponReach.DamageAt).
	Weapons []WeaponReach

	// Unmodeled lists modifier shapes the engine deliberately
	// skipped (deduped, sorted): domains/funcs it cannot target,
	// overload-category effects, and similar.
	Unmodeled []string
	// UnmodeledBy is the fitted types (modules, rigs, the hull,
	// implants, charges) whose effects Unmodeled is about, sorted.
	UnmodeledBy []int64

	// Restricted lists fitted modules whose canFitShip*
	// attributes forbid the current hull. The engine flags them
	// as fit errors; it does not silently drop them.
	Restricted []Restriction

	// OverGroupLimit lists module groups fitted more times than
	// their maxGroupFitted (1544) allows, such as a second
	// synchronizer rig or damage control.
	OverGroupLimit []GroupLimit
}

// GroupLimit is one module group over its per-ship limit.
type GroupLimit struct {
	GroupID int64
	Allowed int
	Fitted  int
}

// Restriction is one ship-restricted module on an
// incompatible hull: the module type plus the ship groups/types
// it may be fitted to.
type Restriction struct {
	TypeID    int64
	NeedGroup []int64
	NeedType  []int64
}

// entityKind classifies one computation entity.
type entityKind int

const (
	entShip entityKind = iota
	entChar
	entSkill
	entModule
	entRig
	entSubsystem
	entDrone
	entMissile
	entCharge
	entImplant
	entFighter
)

// entity is one thing whose attributes get computed: the ship,
// the character pseudo-item, a trained skill, or one fitted type
// (counted Instances times where identity matters). Modules with
// different states fold into separate entities (state is part of
// the fold key); other kinds ignore it.
type entity struct {
	key       string
	kind      entityKind
	typeID    int64
	instances int
	state     string   // module state, "" for other kinds
	abilities []string // a fighter's switched-on abilities
	base      map[int64]float64
	calc      map[int64]float64 // attributes touched by modifiers, post-pass
}

// cycling reports whether the entity's active effects are
// engaged: everything but modules always is; a module only when
// active or overheated. Empty state (no SDE effects to derive
// states from) preserves the historical behavior: cycling.
func (e *entity) cycling() bool {
	if e.kind != entModule {
		return true
	}
	return e.state == "" || e.state == StateActive || e.state == StateOverheated
}

// online reports whether passive/online-category effects apply:
// everything but modules always; a module unless offline.
func (e *entity) online() bool {
	if e.kind != entModule {
		return true
	}
	return e.state != StateOffline
}

func (e *entity) get(snap *Snapshot, attr int64) float64 {
	if v, ok := e.calc[attr]; ok {
		return v
	}
	if v, ok := e.base[attr]; ok {
		return v
	}
	return snap.attrDefault(attr)
}

// application is one (source effect modifier -> target entity)
// binding.
type application struct {
	source   *entity
	target   *entity
	effectID int64
	mod      Modifier
}

// penaltySubject reports whether this application sits in a
// stacking-penalty chain for its attribute: multiplicative op
// (pre or post), non-stackable attribute, fitted module or rig.
// Two Ballistic Control Systems are the pre-multiply case.
// Subsystems, like the hull, skills and implants, are exempt.
func (app application) penaltySubject(snap *Snapshot) bool {
	switch app.mod.Operation {
	case opPreMultiply, opPreDivide, opPostMult, opPostDivide, opPostPercent:
	default:
		return false
	}
	if snap.stackable(app.mod.ModifiedAttr) {
		return false
	}
	switch app.source.kind {
	case entModule, entRig:
		return true
	}
	return false
}

// impact ranks a multiplicative modifier for penalty ordering:
// larger = stronger effect on the value. Multipliers are
// compared as plain ratios, sorted by |factor - 1|.
func impact(op int64, v float64) float64 {
	var factor float64
	switch op {
	case opPreMultiply, opPostMult:
		factor = v
	case opPreDivide, opPostDivide:
		factor = 1 / v
	case opPostPercent:
		factor = 1 + v/100
	default:
		return 0
	}
	if factor <= 0 {
		return math.Inf(1)
	}
	return math.Abs(factor - 1)
}

// multFactor converts one multiplicative application into its
// equivalent multiplier on the target attribute.
func multFactor(op int64, v float64) float64 {
	switch op {
	case opPreMultiply, opPostMult:
		return v
	case opPreDivide, opPostDivide:
		if v != 0 {
			return 1 / v
		}
	case opPostPercent:
		return 1 + v/100
	}
	return 1
}

// stackingEffectiveness is the penalty factor for the n-th
// (0-based) penalized modifier on one attribute.
func stackingEffectiveness(rank int) float64 {
	if rank <= 0 {
		return 1
	}
	r := float64(rank)
	return math.Exp(-r * r / 7.1289)
}

// foldAttribute computes one attribute's final value from its
// base under the bound applications, in dogma stage order.
// Source attribute values come from val, which resolves them to
// their own final values first. Each multiplicative stage
// (pre-multiply, pre-divide, then the post stages together)
// penalizes its own fitted-module modifiers.
func foldAttribute(snap *Snapshot, base float64, apps []application, val func(*entity, int64) float64) float64 {
	value := base

	applyStage := func(ops ...int64) {
		for i := range apps {
			app := &apps[i]
			op := app.mod.Operation
			match := false
			for _, want := range ops {
				if op == want {
					match = true
					break
				}
			}
			if !match {
				continue
			}
			raw := val(app.source, app.mod.ModifyingAttr)
			switch op {
			case opPreAssign, opPostAssign:
				value = raw
			case opModAdd:
				value += raw
			case opModSub:
				value -= raw
			}
		}
	}

	// multStage applies one multiplicative stage: non-penalized
	// multipliers at full strength, penalized ones split into
	// bonus (>1) and penalty (<1) chains, each ordered by impact
	// descending and damped independently.
	multStage := func(ops ...int64) {
		var bonuses, penalties []multEntry
		for i := range apps {
			app := &apps[i]
			op := app.mod.Operation
			if !slices.Contains(ops, op) {
				continue
			}
			v := val(app.source, app.mod.ModifyingAttr)
			factor := multFactor(op, v)
			if !app.penaltySubject(snap) {
				value *= factor
				continue
			}
			entry := multEntry{factor: factor, impact: impact(op, v)}
			if factor >= 1 {
				bonuses = append(bonuses, entry)
			} else {
				penalties = append(penalties, entry)
			}
		}
		for _, chain := range [][]multEntry{bonuses, penalties} {
			sort.SliceStable(chain, func(i, j int) bool { return chain[i].impact > chain[j].impact })
			for rank, entry := range chain {
				value *= 1 + (entry.factor-1)*stackingEffectiveness(rank)
			}
		}
	}

	applyStage(opPreAssign)
	multStage(opPreMultiply)
	multStage(opPreDivide)
	applyStage(opModAdd, opModSub)
	// PostMultiply, PostDivide and PostPercent share one chain.
	multStage(opPostMult, opPostDivide, opPostPercent)
	applyStage(opPostAssign)
	return value
}

// multEntry is one penalty-subject multiplicative modifier
// reduced to a plain multiplier plus its ordering impact.
type multEntry struct {
	factor float64
	impact float64
}

// cloneAttrMap copies a type's base attribute map (nil-safe).
func cloneAttrMap(in map[int64]float64) map[int64]float64 {
	out := make(map[int64]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// finalAttrs returns base + computed attributes for an entity.
func finalAttrs(snap *Snapshot, e *entity) map[int64]float64 {
	out := cloneAttrMap(e.base)
	for attr := range e.calc {
		out[attr] = e.get(snap, attr)
	}
	return out
}

// RestrictionFor reports whether a module may go on the hull: nil
// when it may, else what it needs. The snapshot must hold both types.
func RestrictionFor(snap *Snapshot, shipTypeID, moduleTypeID int64) *Restriction {
	return restrictionFor(snap, shipTypeID, snap.Groups[shipTypeID], moduleTypeID)
}

// restrictionFor returns the restriction for a module on the
// given hull, or nil when the module fits. A module is
// restricted when it carries any canFitShipGroup01-04
// (1298-1301), canFitShipType1-4 (1302-1305) or fitsToShipType
// (1380) attribute; it is
// allowed when the hull's type or group matches any named one.
func restrictionFor(snap *Snapshot, shipTypeID, shipGroup, moduleTypeID int64) *Restriction {
	attrs := snap.Attrs[moduleTypeID]
	if attrs == nil {
		return nil
	}
	r := &Restriction{TypeID: moduleTypeID}
	for a := int64(AttrCanFitShipGroup01); a <= int64(AttrCanFitShipGroup04); a++ {
		if g := int64(attrs[a]); g > 0 {
			r.NeedGroup = append(r.NeedGroup, g)
		}
	}
	for a := int64(AttrCanFitShipType1); a <= int64(AttrCanFitShipType4); a++ {
		if t := int64(attrs[a]); t > 0 {
			r.NeedType = append(r.NeedType, t)
		}
	}
	if t := int64(attrs[AttrFitsToShipType]); t > 0 {
		r.NeedType = append(r.NeedType, t)
	}
	if len(r.NeedGroup) == 0 && len(r.NeedType) == 0 {
		return nil
	}
	for _, t := range r.NeedType {
		if t == shipTypeID {
			return nil
		}
	}
	for _, g := range r.NeedGroup {
		if g == shipGroup {
			return nil
		}
	}
	return r
}

// cycleSeconds is an item's repeat interval in seconds, the basis
// of every per-second rate: duration (73) wins, speed (51) is the
// weapon/drone fallback, and a reactivation delay (669) adds the
// wait before the module can run again.
func cycleSeconds(e *entity, snap *Snapshot) float64 {
	var c float64
	if d := e.get(snap, AttrDuration); d > 0 {
		c = d / 1000
	} else if s := e.get(snap, AttrSpeed); s > 0 {
		c = s / 1000
	} else {
		return 0
	}
	return c + e.get(snap, AttrReactivationDelay)/1000
}

// layerEHP computes a tank layer's EHP per damage type from
// hit points and resonances (EM, thermal, kinetic, explosive
// order). Omni uses the mean resonance; a resonance of 0 or
// less makes that profile immune (infinite EHP).
func layerEHP(hp float64, em, thermal, kinetic, explosive float64) EHPProfile {
	resist := func(r float64) float64 {
		if r <= 0 {
			return math.Inf(1)
		}
		return hp / r
	}
	mean := (em + thermal + kinetic + explosive) / 4
	return EHPProfile{
		Omni:      resist(mean),
		EM:        resist(em),
		Thermal:   resist(thermal),
		Kinetic:   resist(kinetic),
		Explosive: resist(explosive),
	}
}

// capacitorEquilibriumPercent returns the capacitor fraction
// (0-100) at which recharge balances a constant draw: the
// stable equilibrium above the recharge curve's peak point.
// Solves (sqrt(x) - x) = draw*T/(10*C) for the upper root.
func capacitorEquilibriumPercent(capacity, rechargeSecs, draw float64) float64 {
	if capacity <= 0 || rechargeSecs <= 0 || draw <= 0 {
		return 100
	}
	k := draw * rechargeSecs / (10 * capacity)
	if k >= 0.25 {
		return 25 // at the peak; at/above peak draw is unstable
	}
	s := (1 + math.Sqrt(1-4*k)) / 2
	return 100 * s * s
}

// capacitorDepletionSeconds integrates the capacitor curve
// (recharge rate proportional to sqrt(x)-x, peaking at 2.5*C/T)
// against a constant draw, from full, in 50 ms steps. Returns
// the second count at which the capacitor empties, or 0 when
// it effectively never does within the simulated hour.
func capacitorDepletionSeconds(capacity, rechargeSecs, draw float64) float64 {
	if capacity <= 0 || rechargeSecs <= 0 {
		return 0
	}
	const dt = 0.05
	level := capacity
	for t := 0.0; t < 3600; t += dt {
		x := level / capacity
		if x < 0 {
			x = 0
		}
		recharge := (10 * capacity / rechargeSecs) * (math.Sqrt(x) - x)
		level += (recharge - draw) * dt
		if level <= 0 {
			return t + dt
		}
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
