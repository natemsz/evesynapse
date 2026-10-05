package app

// ---------------------------------------------------------------------------
// Fitting simulator stat engine (schema 029, v0.3.21).
//
// computeFit takes a ship, a set of fitted items (modules, rigs,
// drones with quantities), a skill level per skill type, and a
// charge choice per weapon type, loads the dogma data for those
// types from the SDE tables (loadFitSnapshot), and computes the
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
// effects are never penalized. Following pyfa's eos
// (modifiedAttributeDict.__calculateValue), penalized modifiers
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
// OwnerRequiredSkillModifier (drone targets). Domains targetID /
// target / structureID, the EffectStopper func, effect
// categories outside {0 passive, 1 active, 4 online} — except 5 =
// overload, which binds for overheated modules — and charge/group
// shapes that target nothing listed here are skipped and reported
// in fitResult.Unmodeled, never silently dropped. A heatable
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
// so there is no modifier machinery to run — verified against the
// live dump 2026-10-05); their PG/CPU and role bonuses flow through
// the normal modifier machinery.
// ---------------------------------------------------------------------------

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"

	db "evesynapse/internal/db/sqlc"
)

// Dogma attribute IDs the engine reads (dgmAttributeTypes names
// verified against the live dump 2026-10-04).
const (
	fitAttrMass               = 4
	fitAttrCapacitorNeed      = 6
	fitAttrHP                 = 9
	fitAttrPowerOutput        = 11
	fitAttrLowSlots           = 12
	fitAttrMedSlots           = 13
	fitAttrHiSlots            = 14
	fitAttrSpeedFactor        = 20
	fitAttrPower              = 30
	fitAttrMaxVelocity        = 37
	fitAttrCapacity           = 38
	fitAttrCPUOutput          = 48
	fitAttrCPU                = 50
	fitAttrSpeed              = 51
	fitAttrCapacitorRecharge  = 55
	fitAttrDamageMultiplier   = 64
	fitAttrShieldBonus        = 68
	fitAttrAgility            = 70
	fitAttrDuration           = 73
	fitAttrMaxTargetRange     = 76
	fitAttrStructureDamageAmt = 83
	fitAttrArmorDamageAmount  = 84
	fitAttrLauncherSlotsLeft  = 101
	fitAttrTurretSlotsLeft    = 102
	fitAttrResonanceEM        = 113 // generic (hull) EM resonance
	fitAttrResonanceExplosive = 111
	fitAttrResonanceKinetic   = 109
	fitAttrResonanceThermal   = 110
	fitAttrDamageEM           = 114
	fitAttrDamageExplosive    = 116
	fitAttrDamageKinetic      = 117
	fitAttrDamageThermal      = 118
	fitAttrMaxLockedTargets   = 192
	fitAttrScanRadar          = 208
	fitAttrScanLadar          = 209
	fitAttrScanMagnetometric  = 210
	fitAttrScanGravimetric    = 211
	fitAttrMissileDamageMult  = 212
	fitAttrShieldCapacity     = 263
	fitAttrArmorHP            = 265
	fitAttrArmorEM            = 267
	fitAttrArmorExplosive     = 268
	fitAttrArmorKinetic       = 269
	fitAttrArmorThermal       = 270
	fitAttrShieldEM           = 271
	fitAttrShieldExplosive    = 272
	fitAttrShieldKinetic      = 273
	fitAttrShieldThermal      = 274
	fitAttrSkillLevel         = 280
	fitAttrDroneCapacity      = 283
	fitAttrCapacitorCapacity  = 482
	fitAttrShieldRechargeRate = 479
	fitAttrSignatureRadius    = 552
	fitAttrScanResolution     = 564
	fitAttrSpeedBoostFactor   = 567
	fitAttrMassAddition       = 796
	fitAttrUpgradeCapacity    = 1132
	fitAttrRigSlots           = 1137
	fitAttrUpgradeCost        = 1153
	fitAttrDroneBandwidth     = 1271
	fitAttrDroneBandwidthUsed = 1272
	fitAttrDronesSkill        = 3436
	// T3 subsystem slot/hardpoint grants (dgmAttributeTypes,
	// verified against the live dump 2026-10-05): subsystems
	// carry these as plain attributes; the dump's slotModifier
	// effect (3774) has no dogma modifiers, so the grants sum
	// directly instead of flowing through the modifier machinery.
	fitAttrTurretHardPointModifier   = 1368
	fitAttrLauncherHardPointModifier = 1369
	fitAttrHiSlotModifier            = 1374
	fitAttrMedSlotModifier           = 1375
	fitAttrLowSlotModifier           = 1376
	// Ship-restriction attributes (dgmAttributeTypes, verified
	// against the live dump 2026-10-05): a module carrying any of
	// these may only be fitted when the hull matches one of the
	// named ship groups or types. Siege modules point at group
	// 485 (Dreadnought) via canFitShipGroup01; bastion at
	// marauders, triage at carriers, same shape.
	fitAttrCanFitShipGroup01 = 1298
	fitAttrCanFitShipGroup04 = 1301
	fitAttrCanFitShipType1   = 1302
	fitAttrCanFitShipType4   = 1305
	// Cap-warfare amounts (dgmAttributeTypes): neutralizers drain
	// via energyNeutralizerAmount (97); nosferatu via
	// powerTransferAmount (90), which remote capacitor
	// transmitters (group 67) share — group 68 is the nosferatu
	// group, so the engine only counts 68 as offensive drain.
	fitAttrEnergyNeutralizerAmount = 97
	fitAttrPowerTransferAmount     = 90
	fitGroupRemoteCapTransmitter   = 67
	fitGroupNosferatu              = 68
)

// Slot / fitting effect IDs (dgmEffects names verified live).
const (
	fitEffectLoPower        = 11
	fitEffectHiPower        = 12
	fitEffectMedPower       = 13
	fitEffectLauncherFitted = 40
	fitEffectTurretFitted   = 42
	fitEffectRigSlot        = 2663
	fitEffectSubsystemSlot  = 3772
)

// Effect categories (dgmEffects): 5 is the overload/heat
// category — the one heating a module engages.
const fitCatOverload = 5

// Module states: the values fitItemInput.State and
// fitDocItem.States carry. pyfa/eos semantics —
//   - offline:    fitted but contributes nothing (no CPU/PG, no effects)
//   - online:     powered; passive + online-category effects apply,
//     active effects don't, nothing cycles
//   - active:     cycling; passive + online + active effects apply
//   - overheated: active plus the module's overload effects
const (
	fitStateOnline     = "online"
	fitStateActive     = "active"
	fitStateOffline    = "offline"
	fitStateOverheated = "overheated"
)

// Dogma operation codes (see the header comment for evidence).
const (
	fitOpPreAssign   = -1
	fitOpPreMultiply = 0
	fitOpPreDivide   = 1
	fitOpModAdd      = 2
	fitOpModSub      = 3
	fitOpPostMult    = 4
	fitOpPostDivide  = 5
	fitOpPostPercent = 6
	fitOpPostAssign  = 7
)

// fitAttrMeta is one dgmAttributeTypes row reduced to what the
// engine needs: stacking behaviour and the default value an
// attribute carries when no type row sets it.
type fitAttrMeta struct {
	Stackable bool
	Default   float64
}

// fitModifier is one decoded dgmEffects modifier.
type fitModifier struct {
	Domain        string
	Func          string
	ModifiedAttr  int64
	ModifyingAttr int64
	Operation     int64
	GroupID       int64
	SkillTypeID   int64
}

// fitEffect is one modifier-bearing effect.
type fitEffect struct {
	Name      string
	Category  int64
	Modifiers []fitModifier
}

// fitSkillReq is one (type requires skill at level) row.
type fitSkillReq struct {
	SkillTypeID int64
	Level       int64
}

// fitPhysics is one sde_type_physics row.
type fitPhysics struct {
	Mass     float64
	Volume   float64
	Capacity float64
}

// fitSnapshot is the engine's read-only view of the dogma data
// for a closed set of types (ship, items, charges, and every
// skill reachable from their requirements/effect selectors).
type fitSnapshot struct {
	attrs        map[int64]map[int64]float64 // type -> attribute -> base value
	physics      map[int64]fitPhysics
	groups       map[int64]int64 // type -> invGroups group ID
	meta         map[int64]fitAttrMeta
	effects      map[int64]*fitEffect // only modifier-bearing effects
	typeEffects  map[int64][]int64    // type -> effect IDs (sorted)
	requirements map[int64][]fitSkillReq
}

// attrDefault returns the value an attribute starts from when a
// type carries no row for it (dgmAttributeTypes defaultValue,
// with hard floors for the multiplicative identities).
func (snap *fitSnapshot) attrDefault(attr int64) float64 {
	if m, ok := snap.meta[attr]; ok {
		return m.Default
	}
	switch attr {
	case fitAttrDamageMultiplier, fitAttrMissileDamageMult:
		return 1
	}
	return 0
}

// stackable reports whether modifiers on an attribute dodge the
// stacking-penalty group (dgmAttributeTypes stackable flag; an
// unknown attribute is treated as stackable so penalties never
// apply on missing data).
func (snap *fitSnapshot) stackable(attr int64) bool {
	if m, ok := snap.meta[attr]; ok {
		return m.Stackable
	}
	return true
}

// requiresSkill reports whether a type's requirement rows name
// the given skill.
func (snap *fitSnapshot) requiresSkill(typeID, skillTypeID int64) bool {
	for _, r := range snap.requirements[typeID] {
		if r.SkillTypeID == skillTypeID {
			return true
		}
	}
	return false
}

// hasEffect reports whether a type carries the given effect.
func (snap *fitSnapshot) hasEffect(typeID, effectID int64) bool {
	for _, id := range snap.typeEffects[typeID] {
		if id == effectID {
			return true
		}
	}
	return false
}

// fitTypeEffectCats lists the effect categories a type carries.
func (snap *fitSnapshot) fitTypeEffectCats(typeID int64) map[int64]bool {
	out := make(map[int64]bool)
	for _, eid := range snap.typeEffects[typeID] {
		if eff := snap.effects[eid]; eff != nil {
			out[eff.Category] = true
		}
	}
	return out
}

// fitModuleValidStates lists the states a fitted module may take,
// derived from SDE data, never per-module hardcoding: rigs,
// subsystems, drones and cargo have no states; a module is
// activatable with an active-category effect — or as a weapon
// (turret/launcher fitted markers are passive in SDE, but weapons
// cycle via F1 in-game); overheatable only with an
// overload-category effect on top of that (heat is applied to a
// cycling module).
func fitModuleValidStates(snap *fitSnapshot, typeID int64) []string {
	if snap.hasEffect(typeID, fitEffectRigSlot) || snap.hasEffect(typeID, fitEffectSubsystemSlot) {
		return nil
	}
	if snap.attrs[typeID][fitAttrDroneBandwidthUsed] > 0 {
		return nil
	}
	cats := snap.fitTypeEffectCats(typeID)
	if len(cats) == 0 {
		return nil
	}
	activatable := cats[1] ||
		snap.hasEffect(typeID, fitEffectTurretFitted) ||
		snap.hasEffect(typeID, fitEffectLauncherFitted)
	states := []string{fitStateOffline, fitStateOnline}
	if activatable {
		states = append(states, fitStateActive)
		if cats[fitCatOverload] {
			states = append(states, fitStateOverheated)
		}
	}
	return states
}

// fitDefaultModuleState is the state a fresh module takes: active
// when it can cycle, online otherwise. This preserves the
// engine's historical behavior (everything online and cycling)
// for fits that predate states.
func fitDefaultModuleState(snap *fitSnapshot, typeID int64) string {
	cats := snap.fitTypeEffectCats(typeID)
	if cats[1] ||
		snap.hasEffect(typeID, fitEffectTurretFitted) ||
		snap.hasEffect(typeID, fitEffectLauncherFitted) {
		return fitStateActive
	}
	return fitStateOnline
}

// fitNormalizeModuleState resolves "" to the type default and
// demotes states the type cannot take (stale docs, bad input) to
// the default. Non-state kinds get "".
func fitNormalizeModuleState(snap *fitSnapshot, typeID int64, state string) string {
	valid := fitModuleValidStates(snap, typeID)
	if len(valid) == 0 {
		return ""
	}
	for _, v := range valid {
		if v == state {
			return state
		}
	}
	return fitDefaultModuleState(snap, typeID)
}

// fitSourceCategories lists the effect categories that bind for
// one entity under its module state (pyfa semantics): offline
// modules bind nothing; online skips active and overload;
// active adds the active category; overheated adds overload.
func fitSourceCategories(e *fitEntity) map[int64]bool {
	if e.kind != fitEntModule {
		return map[int64]bool{0: true, 1: true, 4: true}
	}
	switch e.state {
	case fitStateOffline:
		return nil
	case fitStateOnline:
		return map[int64]bool{0: true, 4: true}
	case fitStateOverheated:
		return map[int64]bool{0: true, 1: true, 4: true, fitCatOverload: true}
	default:
		return map[int64]bool{0: true, 1: true, 4: true}
	}
}

// fitStateAttrKey keys per-(type, state) attribute maps.
func fitStateAttrKey(typeID int64, state string) string {
	return strconv.FormatInt(typeID, 10) + "\x00" + state
}

// loadFitSnapshot loads the dogma rows for the given types and
// closes over the skills they touch: requirements (recursive)
// and skill selectors in any carried effect's modifiers. The
// extra rounds discover skill types so their own attributes and
// effects are present when the engine runs.
func loadFitSnapshot(ctx context.Context, q *db.Queries, typeIDs []int64) (*fitSnapshot, error) {
	snap := &fitSnapshot{
		attrs:        make(map[int64]map[int64]float64),
		physics:      make(map[int64]fitPhysics),
		groups:       make(map[int64]int64),
		meta:         make(map[int64]fitAttrMeta),
		effects:      make(map[int64]*fitEffect),
		typeEffects:  make(map[int64][]int64),
		requirements: make(map[int64][]fitSkillReq),
	}

	loadedTypes := make(map[int64]bool)
	loadedEffects := make(map[int64]bool)
	pending := dedupeSortedIDs(typeIDs)

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
			m := snap.attrs[r.TypeID]
			if m == nil {
				m = make(map[int64]float64)
				snap.attrs[r.TypeID] = m
			}
			m[r.AttributeID] = r.Value
		}
		physRows, err := q.ListSDETypePhysicsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type physics: %w", err)
		}
		for _, r := range physRows {
			snap.physics[r.TypeID] = fitPhysics{Mass: r.Mass, Volume: r.Volume, Capacity: r.Capacity}
		}
		groupRows, err := q.ListSDETypeGroupsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type groups: %w", err)
		}
		for _, r := range groupRows {
			snap.groups[r.TypeID] = r.GroupID
		}
		effectLinks, err := q.ListSDETypeEffectsByIDs(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load type effects: %w", err)
		}
		for _, r := range effectLinks {
			snap.typeEffects[r.TypeID] = append(snap.typeEffects[r.TypeID], r.EffectID)
		}
		reqRows, err := q.ListSDERequirementsByTypes(ctx, fresh)
		if err != nil {
			return nil, fmt.Errorf("load requirements: %w", err)
		}
		for _, r := range reqRows {
			snap.requirements[r.TypeID] = append(snap.requirements[r.TypeID],
				fitSkillReq{SkillTypeID: r.SkillTypeID, Level: r.Level})
		}

		// Effects carried by the fresh types (only ones the
		// importer stored, i.e. carrying modifiers).
		var newEffects []int64
		for _, id := range fresh {
			for _, eid := range snap.typeEffects[id] {
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
				snap.effects[r.EffectID] = &fitEffect{Name: r.Name, Category: r.Category}
			}
			modRows, err := q.ListSDEEffectModifiersByIDs(ctx, newEffects)
			if err != nil {
				return nil, fmt.Errorf("load effect modifiers: %w", err)
			}
			for _, r := range modRows {
				eff := snap.effects[r.EffectID]
				if eff == nil {
					eff = &fitEffect{}
					snap.effects[r.EffectID] = eff
				}
				eff.Modifiers = append(eff.Modifiers, fitModifier{
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
			for attr := range snap.attrs[id] {
				attrIDs = append(attrIDs, attr)
			}
		}
		for _, eff := range snap.effects {
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
				snap.meta[r.AttributeID] = fitAttrMeta{
					Stackable: r.Stackable != 0,
					Default:   r.DefaultValue,
				}
			}
		}

		// Next round: skills referenced by requirements or by
		// modifier selectors anywhere in the loaded effects.
		var next []int64
		for _, id := range fresh {
			for _, r := range snap.requirements[id] {
				if !loadedTypes[r.SkillTypeID] {
					next = append(next, r.SkillTypeID)
				}
			}
		}
		for _, eff := range snap.effects {
			for _, m := range eff.Modifiers {
				if m.SkillTypeID != 0 && !loadedTypes[m.SkillTypeID] {
					next = append(next, m.SkillTypeID)
				}
			}
		}
		pending = dedupeSortedIDs(next)
	}

	for _, ids := range snap.typeEffects {
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

// fitAllVSkillLevels returns the "All V" skill map for a fit:
// every skill reachable from the ship's and items' requirements
// (recursively) and from effect skill selectors, at level 5.
func fitAllVSkillLevels(snap *fitSnapshot, shipTypeID int64, itemTypeIDs []int64) map[int64]int {
	skills := make(map[int64]bool)
	queue := append([]int64{shipTypeID}, itemTypeIDs...)
	for len(queue) > 0 {
		typeID := queue[0]
		queue = queue[1:]
		for _, r := range snap.requirements[typeID] {
			if !skills[r.SkillTypeID] {
				skills[r.SkillTypeID] = true
				queue = append(queue, r.SkillTypeID)
			}
		}
		for _, eid := range snap.typeEffects[typeID] {
			if eff := snap.effects[eid]; eff != nil {
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

// fitItemInput is one fitted item line: a type, how many of
// it the fit carries (drones especially), and — for modules —
// the module state ("", online/active/offline/overheated; ""
// resolves to the type's default).
type fitItemInput struct {
	TypeID   int64
	Quantity int
	State    string
}

// fitEHPProfile is effective HP against a damage profile.
type fitEHPProfile struct {
	Omni      float64
	EM        float64
	Thermal   float64
	Kinetic   float64
	Explosive float64
}

// fitResult is the computed fit. Floats are raw dogma units
// (times in seconds, capacitor in GJ); counts are integers.
type fitResult struct {
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

	CapacitorCapacity      float64
	CapacitorPeakRecharge  float64 // GJ/s at the curve's peak
	CapacitorDraw          float64 // GJ/s with everything cycling
	CapacitorStable        bool
	CapacitorStablePercent float64 // equilibrium % when stable (pyfa's cap state)
	CapacitorDepletionSecs float64 // approx seconds to empty; 0 when stable

	TurretDPS  float64
	MissileDPS float64
	DroneDPS   float64
	DPS        float64

	// Volley damage per weapon cycle, broken down like DPS.
	TurretVolley  float64
	MissileVolley float64
	DroneVolley   float64
	Volley        float64

	// Capacitor warfare: neutralizer and nosferatu drain, per
	// cycle and per second, from fitted modules.
	NeutDrainPerCycle float64
	NeutDrainPerSec   float64
	NosDrainPerCycle  float64
	NosDrainPerSec    float64

	ShieldHP float64
	ArmorHP  float64
	HullHP   float64

	ShieldEHP fitEHPProfile
	ArmorEHP  fitEHPProfile
	HullEHP   fitEHPProfile
	EHP       fitEHPProfile

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
	// fitStateAttrKey — so a tooltip can show an overheated
	// module's heated numbers rather than its resting ones.
	StateAttrs map[string]map[int64]float64

	// Unmodeled lists modifier shapes the engine deliberately
	// skipped (deduped, sorted): domains/funcs it cannot target,
	// overload-category effects, and similar.
	Unmodeled []string

	// Restricted lists fitted modules whose canFitShip*
	// attributes forbid the current hull. The engine flags them
	// as fit errors; it does not silently drop them.
	Restricted []fitRestriction
}

// fitRestriction is one ship-restricted module on an
// incompatible hull: the module type plus the ship groups/types
// it may be fitted to.
type fitRestriction struct {
	TypeID    int64
	NeedGroup []int64
	NeedType  []int64
}

// fitEntityKind classifies one computation entity.
type fitEntityKind int

const (
	fitEntShip fitEntityKind = iota
	fitEntChar
	fitEntSkill
	fitEntModule
	fitEntRig
	fitEntSubsystem
	fitEntDrone
	fitEntMissile
	fitEntCharge
	fitEntImplant
)

// fitEntity is one thing whose attributes get computed: the ship,
// the character pseudo-item, a trained skill, or one fitted type
// (counted Instances times where identity matters). Modules with
// different states fold into separate entities (state is part of
// the fold key); other kinds ignore it.
type fitEntity struct {
	key       string
	kind      fitEntityKind
	typeID    int64
	instances int
	state     string // module state, "" for other kinds
	base      map[int64]float64
	calc      map[int64]float64 // attributes touched by modifiers, post-pass
}

// cycling reports whether the entity's active effects are
// engaged: everything but modules always is; a module only when
// active or overheated. Empty state (no SDE effects to derive
// states from) preserves the historical behavior: cycling.
func (e *fitEntity) cycling() bool {
	if e.kind != fitEntModule {
		return true
	}
	return e.state == "" || e.state == fitStateActive || e.state == fitStateOverheated
}

// online reports whether passive/online-category effects apply:
// everything but modules always; a module unless offline.
func (e *fitEntity) online() bool {
	if e.kind != fitEntModule {
		return true
	}
	return e.state != fitStateOffline
}

func (e *fitEntity) get(snap *fitSnapshot, attr int64) float64 {
	if v, ok := e.calc[attr]; ok {
		return v
	}
	if v, ok := e.base[attr]; ok {
		return v
	}
	return snap.attrDefault(attr)
}

// fitApplication is one (source effect modifier -> target entity)
// binding.
type fitApplication struct {
	source   *fitEntity
	target   *fitEntity
	effectID int64
	mod      fitModifier
}

// penaltySubject reports whether this application sits in the
// stacking-penalty group for its attribute: multiplicative op,
// non-stackable attribute, fitted-module source.
func (app fitApplication) penaltySubject(snap *fitSnapshot) bool {
	switch app.mod.Operation {
	case fitOpPostMult, fitOpPostDivide, fitOpPostPercent:
	default:
		return false
	}
	if snap.stackable(app.mod.ModifiedAttr) {
		return false
	}
	switch app.source.kind {
	case fitEntModule, fitEntRig, fitEntSubsystem:
		return true
	}
	return false
}

// impact ranks a multiplicative modifier for penalty ordering:
// larger = stronger effect on the value. Multipliers are
// compared as plain ratios (pyfa eos sorts by |factor - 1|).
func fitImpact(op int64, v float64) float64 {
	var factor float64
	switch op {
	case fitOpPostMult:
		factor = v
	case fitOpPostDivide:
		factor = 1 / v
	case fitOpPostPercent:
		factor = 1 + v/100
	default:
		return 0
	}
	if factor <= 0 {
		return math.Inf(1)
	}
	return math.Abs(factor - 1)
}

// fitMultFactor converts one multiplicative application into its
// equivalent multiplier on the target attribute.
func fitMultFactor(op int64, v float64) float64 {
	switch op {
	case fitOpPostMult:
		return v
	case fitOpPostDivide:
		if v != 0 {
			return 1 / v
		}
	case fitOpPostPercent:
		return 1 + v/100
	}
	return 1
}

// stackingEffectiveness is the penalty factor for the n-th
// (0-based) penalized modifier on one attribute.
func fitStackingEffectiveness(rank int) float64 {
	if rank <= 0 {
		return 1
	}
	r := float64(rank)
	return math.Exp(-r * r / 7.1289)
}

// foldAttribute computes one attribute's final value from its
// base under the bound applications, in dogma stage order.
// Source attribute values are read live, so earlier stages feed
// later ones (skill pre-multipliers before percent bonuses).
// Multiplicative stages follow pyfa eos: non-penalized
// multipliers apply at full strength; penalized ones are split
// into bonus (>1) and penalty (<1) chains, each ordered by
// impact descending and damped independently. The caller passes
// the base value explicitly: fold passes always recompute from
// base, never from an earlier pass result.
func foldAttribute(snap *fitSnapshot, target *fitEntity, base float64, apps []fitApplication) float64 {
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
			raw := app.source.get(snap, app.mod.ModifyingAttr)
			switch op {
			case fitOpPreAssign, fitOpPostAssign:
				value = raw
			case fitOpPreMultiply:
				value *= raw
			case fitOpPreDivide:
				if raw != 0 {
					value /= raw
				}
			case fitOpModAdd:
				value += raw
			case fitOpModSub:
				value -= raw
			}
		}
	}

	applyStage(fitOpPreAssign)
	applyStage(fitOpPreMultiply)
	applyStage(fitOpPreDivide)
	applyStage(fitOpModAdd, fitOpModSub)

	// Multiplicative stages (PostMultiply, PostDivide,
	// PostPercent): convert to plain multipliers, apply
	// non-penalized ones fully, then the penalized chains.
	var bonuses, penalties []fitMultEntry
	for i := range apps {
		app := &apps[i]
		op := app.mod.Operation
		if op != fitOpPostMult && op != fitOpPostDivide && op != fitOpPostPercent {
			continue
		}
		v := app.source.get(snap, app.mod.ModifyingAttr)
		factor := fitMultFactor(op, v)
		if !app.penaltySubject(snap) {
			value *= factor
			continue
		}
		entry := fitMultEntry{factor: factor, impact: fitImpact(op, v)}
		if factor >= 1 {
			bonuses = append(bonuses, entry)
		} else {
			penalties = append(penalties, entry)
		}
	}
	for _, chain := range [][]fitMultEntry{bonuses, penalties} {
		sort.SliceStable(chain, func(i, j int) bool { return chain[i].impact > chain[j].impact })
		for rank, entry := range chain {
			value *= 1 + (entry.factor-1)*fitStackingEffectiveness(rank)
		}
	}

	applyStage(fitOpPostAssign)
	return value
}

// fitMultEntry is one penalty-subject multiplicative modifier
// reduced to a plain multiplier plus its ordering impact.
type fitMultEntry struct {
	factor float64
	impact float64
}

// computeFit runs the stat engine over one fit. charges maps a
// weapon's type ID to its loaded charge type ID. Skill levels
// are clamped to 0..5; absent skills are untrained. implants are
// the active clone's implant type IDs: they become char-located
// entities so their own effects (ItemModifier on the ship, the
// charID LocationGroupModifier that builds pirate set bonuses)
// flow through the same three-pass fold as everything else.
// Implant sources never join the stacking-penalty group —
// pirate set totals are documented unpenalized (a full
// High-grade Snake set is +24.73% velocity, which only the
// unpenalized product reproduces).
func computeFit(snap *fitSnapshot, shipTypeID int64, items []fitItemInput, levels map[int64]int, charges map[int64]int64, implants []int64) *fitResult {
	res := &fitResult{
		ShipTypeID: shipTypeID,
		ItemAttrs:  make(map[int64]map[int64]float64),
		StateAttrs: make(map[string]map[int64]float64),
	}
	unmodeled := make(map[string]bool)
	noteUnmodeled := func(s string) { unmodeled[s] = true }

	// --- Entities -------------------------------------------------
	shipBase := cloneAttrMap(snap.attrs[shipTypeID])
	if phys, ok := snap.physics[shipTypeID]; ok && phys.Mass > 0 {
		if _, has := shipBase[fitAttrMass]; !has {
			shipBase[fitAttrMass] = phys.Mass
		}
	}
	ship := &fitEntity{key: "ship", kind: fitEntShip, typeID: shipTypeID, instances: 1, base: shipBase, calc: map[int64]float64{}}
	charEnt := &fitEntity{key: "char", kind: fitEntChar, base: map[int64]float64{}, calc: map[int64]float64{}}

	entities := []*fitEntity{ship, charEnt}
	byType := map[int64]*fitEntity{shipTypeID: ship}
	skillEnts := make(map[int64]*fitEntity)

	// Module/rig/drone entities per fitted type (instances fold
	// duplicates of the same type into one entity with a count).
	// Modules fold by (type, state): two identical modules in
	// different states are separate entities so each binds only
	// the effect categories its state allows.
	var fitted []*fitEntity // modules, rigs, subsystems, drones
	fittedByType := make(map[int64][]*fitEntity)
	for _, it := range items {
		if it.TypeID == shipTypeID {
			continue
		}
		qty := it.Quantity
		kind := fitEntModule
		switch {
		case snap.hasEffect(it.TypeID, fitEffectRigSlot):
			kind = fitEntRig
		case snap.hasEffect(it.TypeID, fitEffectSubsystemSlot):
			kind = fitEntSubsystem
		case snap.attrs[it.TypeID][fitAttrDroneBandwidthUsed] > 0:
			kind = fitEntDrone
		}
		if qty <= 0 {
			qty = 1
			if kind == fitEntDrone {
				qty = 0 // a drone line with no count carries nothing
			}
		}
		state := ""
		if kind == fitEntModule {
			state = fitNormalizeModuleState(snap, it.TypeID, it.State)
		}
		var ent *fitEntity
		for _, e := range fittedByType[it.TypeID] {
			if e.state == state {
				ent = e
				break
			}
		}
		if ent != nil {
			ent.instances += qty
			continue
		}
		ent = &fitEntity{
			key:       fmt.Sprintf("fit:%d:%s", it.TypeID, state),
			kind:      kind,
			typeID:    it.TypeID,
			instances: qty,
			state:     state,
			base:      cloneAttrMap(snap.attrs[it.TypeID]),
			calc:      map[int64]float64{},
		}
		fittedByType[it.TypeID] = append(fittedByType[it.TypeID], ent)
		fitted = append(fitted, ent)
		entities = append(entities, ent)
		byType[it.TypeID] = ent
	}

	// Charge entities (and missile entities for launchers),
	// paired with the weapon type that carries them. A charge is
	// only modeled when at least one weapon of its type is
	// cycling — a loaded charge in an offline or inactive weapon
	// does nothing (the doc still remembers it).
	weaponCharge := make(map[int64]*fitEntity) // weapon type -> charge/missile entity
	for weaponType, chargeType := range charges {
		ents := fittedByType[weaponType]
		if len(ents) == 0 || chargeType == 0 {
			continue
		}
		cycling := false
		for _, e := range ents {
			if e.cycling() {
				cycling = true
				break
			}
		}
		if !cycling {
			continue
		}
		kind := fitEntCharge
		if snap.hasEffect(weaponType, fitEffectLauncherFitted) {
			kind = fitEntMissile
		}
		ent := &fitEntity{
			key:       fmt.Sprintf("charge:%d", chargeType),
			kind:      kind,
			typeID:    chargeType,
			instances: 1,
			base:      cloneAttrMap(snap.attrs[chargeType]),
			calc:      map[int64]float64{},
		}
		weaponCharge[weaponType] = ent
		entities = append(entities, ent)
		byType[chargeType] = ent
	}

	// Trained skills as pseudo-items (skillLevel set from input).
	for skillID, lvl := range levels {
		if lvl <= 0 {
			continue
		}
		if lvl > 5 {
			lvl = 5
		}
		base := cloneAttrMap(snap.attrs[skillID])
		base[fitAttrSkillLevel] = float64(lvl)
		ent := &fitEntity{
			key:       fmt.Sprintf("skill:%d", skillID),
			kind:      fitEntSkill,
			typeID:    skillID,
			instances: 1,
			base:      base,
			calc:      map[int64]float64{},
		}
		skillEnts[skillID] = ent
		entities = append(entities, ent)
		byType[skillID] = ent
	}

	// Implants as char-located pseudo-items (one entity per
	// implant type; slots are unique so instances stays 1).
	var implantEnts []*fitEntity
	seenImplant := make(map[int64]bool)
	for _, implantID := range implants {
		if implantID <= 0 || implantID == shipTypeID || seenImplant[implantID] {
			continue
		}
		seenImplant[implantID] = true
		ent := &fitEntity{
			key:       fmt.Sprintf("implant:%d", implantID),
			kind:      fitEntImplant,
			typeID:    implantID,
			instances: 1,
			base:      cloneAttrMap(snap.attrs[implantID]),
			calc:      map[int64]float64{},
		}
		implantEnts = append(implantEnts, ent)
		entities = append(entities, ent)
		byType[implantID] = ent
	}

	// --- Ship restrictions -----------------------------------------
	// Modules carrying canFitShipGroup*/canFitShipType* may only
	// fly on the named hulls. Flag violations as fit errors.
	shipGroup := snap.groups[shipTypeID]
	for _, ent := range fitted {
		if r := fitRestrictionFor(snap, shipTypeID, shipGroup, ent.typeID); r != nil {
			res.Restricted = append(res.Restricted, *r)
		}
	}

	// --- Modifier binding ------------------------------------------
	// atShip are the entities sitting at the ship's location for
	// Location* selectors: fitted equipment plus launched-craft
	// types (drones, missiles).
	atShip := make([]*fitEntity, 0, len(fitted)+len(weaponCharge))
	atShip = append(atShip, fitted...)
	for _, ent := range weaponCharge {
		atShip = append(atShip, ent)
	}
	var drones []*fitEntity
	for _, ent := range fitted {
		if ent.kind == fitEntDrone {
			drones = append(drones, ent)
		}
	}

	var apps []fitApplication
	bind := func(source *fitEntity, effectID int64, eff *fitEffect, m fitModifier) {
		if m.ModifiedAttr == fitAttrSkillLevel {
			return // skill levels are engine inputs
		}
		targets := func(list ...*fitEntity) {
			for _, t := range list {
				if t != nil {
					apps = append(apps, fitApplication{source: source, target: t, effectID: effectID, mod: m})
				}
			}
		}
		shape := fmt.Sprintf("%s/%s (effect %d %s)", m.Func, m.Domain, effectID, eff.Name)
		switch m.Func {
		case "ItemModifier":
			switch m.Domain {
			case "shipID":
				targets(ship)
			case "charID":
				targets(charEnt)
			case "itemID":
				targets(source)
			case "otherID":
				// A charge modifier reaching its host weapon.
				if source.kind == fitEntCharge || source.kind == fitEntMissile {
					for weaponType, ent := range weaponCharge {
						if ent == source {
							targets(fittedByType[weaponType]...)
						}
					}
				} else {
					noteUnmodeled(shape + ": no charge host")
				}
			default:
				noteUnmodeled(shape + ": domain not modeled")
			}
		case "LocationModifier":
			switch m.Domain {
			case "shipID":
				for _, t := range atShip {
					switch t.kind {
					case fitEntModule, fitEntRig, fitEntSubsystem:
						targets(t)
					}
				}
			case "charID":
				// Attribute implants and similar char-located
				// effects land on the character pseudo-item.
				// Nothing downstream reads charEnt stats, so
				// this is modeling completeness, not behavior.
				targets(charEnt)
			default:
				noteUnmodeled(shape + ": domain not modeled")
				return
			}
		case "LocationGroupModifier":
			switch m.Domain {
			case "shipID":
				for _, t := range atShip {
					if t.kind == fitEntCharge {
						continue // contained in its weapon, not at the ship
					}
					if snap.groups[t.typeID] == m.GroupID && m.GroupID != 0 {
						targets(t)
					}
				}
			case "charID":
				// Pirate implant set bonuses: each set implant
				// carries an effect like setBonusSerpentis that
				// pre-multiplies the set's bonus attribute on
				// every implant of the set group plugged into
				// the character (SDE: implantSet* attributes,
				// e.g. 802 implantSetSerpentis -> 315
				// velocityBonus, operator 0 = pre-multiply).
				for _, t := range implantEnts {
					if snap.groups[t.typeID] == m.GroupID && m.GroupID != 0 {
						targets(t)
					}
				}
			default:
				noteUnmodeled(shape + ": domain not modeled")
				return
			}
		case "LocationRequiredSkillModifier":
			if m.Domain != "shipID" {
				noteUnmodeled(shape + ": domain not modeled")
				return
			}
			for _, t := range atShip {
				if t.kind == fitEntCharge {
					continue
				}
				if m.SkillTypeID != 0 && snap.requiresSkill(t.typeID, m.SkillTypeID) {
					targets(t)
				}
			}
		case "OwnerRequiredSkillModifier":
			if m.Domain != "charID" {
				noteUnmodeled(shape + ": domain not modeled")
				return
			}
			for _, t := range drones {
				if m.SkillTypeID != 0 && snap.requiresSkill(t.typeID, m.SkillTypeID) {
					targets(t)
				}
			}
		default:
			noteUnmodeled(shape + ": modifier func not modeled")
		}
	}

	// Sources in a fixed order (ship, char, skills by type,
	// fitted items, charges) so PreAssign outcomes are stable.
	sources := make([]*fitEntity, 0, len(entities))
	sources = append(sources, ship)
	skillIDs := make([]int64, 0, len(skillEnts))
	for id := range skillEnts {
		skillIDs = append(skillIDs, id)
	}
	sort.Slice(skillIDs, func(i, j int) bool { return skillIDs[i] < skillIDs[j] })
	for _, id := range skillIDs {
		sources = append(sources, skillEnts[id])
	}
	for _, ent := range implantEnts {
		sources = append(sources, ent)
	}
	for _, ent := range fitted {
		sources = append(sources, ent)
	}
	for _, ent := range weaponCharge {
		sources = append(sources, ent)
	}

	for _, source := range sources {
		// Effect categories bind per module state (pyfa
		// semantics): an offline module binds nothing, an
		// online-but-inactive one skips active and overload,
		// overheated adds the overload category.
		allowed := fitSourceCategories(source)
		if allowed == nil {
			continue
		}
		for _, effectID := range snap.typeEffects[source.typeID] {
			eff := snap.effects[effectID]
			if eff == nil {
				continue // carries no modifiers; nothing to apply
			}
			if !allowed[eff.Category] {
				// A heatable module that simply isn't heated is
				// a fitting choice, not a modeling gap — skip
				// silently so the heat note only fires for
				// genuinely unmodeled categories.
				if eff.Category == fitCatOverload && source.kind == fitEntModule {
					continue
				}
				noteUnmodeled(fmt.Sprintf("effect %d %s: category %d skipped (overload/system/target mechanics not modeled)", effectID, eff.Name, eff.Category))
				continue
			}
			for _, m := range eff.Modifiers {
				// Instances of the same fitted type bind once per
				// instance so stacking penalties see each copy.
				n := 1
				if source.kind == fitEntModule || source.kind == fitEntRig || source.kind == fitEntSubsystem {
					n = source.instances
					if n < 1 {
						n = 1
					}
				}
				for i := 0; i < n; i++ {
					bind(source, effectID, eff, m)
				}
			}
		}
	}

	// --- Fold passes ------------------------------------------------
	// Group applications per (target, attribute), then recompute
	// from base a few times so cross-entity chains converge
	// (skill -> ship bonus attribute -> module percent).
	type appKey struct {
		target *fitEntity
		attr   int64
	}
	grouped := make(map[appKey][]fitApplication)
	for _, app := range apps {
		k := appKey{app.target, app.mod.ModifiedAttr}
		grouped[k] = append(grouped[k], app)
	}
	for pass := 0; pass < 3; pass++ {
		for k, list := range grouped {
			base, ok := k.target.base[k.attr]
			if !ok {
				base = snap.attrDefault(k.attr)
			}
			k.target.calc[k.attr] = foldAttribute(snap, k.target, base, list)
		}
	}

	res.ShipAttrs = finalAttrs(snap, ship)
	for _, ent := range entities {
		if ent == ship || ent.kind == fitEntSkill || ent == charEnt {
			continue
		}
		fa := finalAttrs(snap, ent)
		if _, ok := res.ItemAttrs[ent.typeID]; !ok {
			res.ItemAttrs[ent.typeID] = fa
		}
		if ent.kind == fitEntModule && ent.state != "" {
			res.StateAttrs[fitStateAttrKey(ent.typeID, ent.state)] = fa
		}
	}

	// --- Derived stats ----------------------------------------------
	sg := func(attr int64) float64 { return ship.get(snap, attr) }

	// Resources. Offline modules are fitted but dark: no PG,
	// no CPU, no effects.
	for _, ent := range fitted {
		if ent.kind == fitEntDrone || !ent.online() {
			continue
		}
		res.PowergridUsed += ent.get(snap, fitAttrPower) * float64(maxInt(ent.instances, 1))
		res.CPUUsed += ent.get(snap, fitAttrCPU) * float64(maxInt(ent.instances, 1))
	}
	res.PowergridMax = sg(fitAttrPowerOutput)
	res.CPUMax = sg(fitAttrCPUOutput)

	// Slots and hardpoints.
	res.HighSlots = int(sg(fitAttrHiSlots))
	res.MediumSlots = int(sg(fitAttrMedSlots))
	res.LowSlots = int(sg(fitAttrLowSlots))
	res.RigSlots = int(sg(fitAttrRigSlots))
	res.TurretHardpoints = int(sg(fitAttrTurretSlotsLeft))
	res.LauncherHardpoints = int(sg(fitAttrLauncherSlotsLeft))
	// T3 strategic cruisers: the hull has no slots of its own;
	// fitted subsystems grant them through the *SlotModifier
	// attributes above (plain data, summed directly).
	for _, ent := range fitted {
		if ent.kind != fitEntSubsystem {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		res.HighSlots += int(ent.base[fitAttrHiSlotModifier] * n)
		res.MediumSlots += int(ent.base[fitAttrMedSlotModifier] * n)
		res.LowSlots += int(ent.base[fitAttrLowSlotModifier] * n)
		res.TurretHardpoints += int(ent.base[fitAttrTurretHardPointModifier] * n)
		res.LauncherHardpoints += int(ent.base[fitAttrLauncherHardPointModifier] * n)
	}
	for _, ent := range fitted {
		n := maxInt(ent.instances, 1)
		switch {
		case ent.kind == fitEntRig:
			res.RigSlotsUsed += n
			res.CalibrationUsed += ent.get(snap, fitAttrUpgradeCost) * float64(n)
		case ent.kind == fitEntModule:
			switch {
			case snap.hasEffect(ent.typeID, fitEffectHiPower):
				res.HighSlotsUsed += n
			case snap.hasEffect(ent.typeID, fitEffectMedPower):
				res.MediumSlotsUsed += n
			case snap.hasEffect(ent.typeID, fitEffectLoPower):
				res.LowSlotsUsed += n
			}
			if snap.hasEffect(ent.typeID, fitEffectTurretFitted) {
				res.TurretHardpointsUsed += n
			}
			if snap.hasEffect(ent.typeID, fitEffectLauncherFitted) {
				res.LauncherHardpointsUsed += n
			}
		}
	}
	res.CalibrationMax = sg(fitAttrUpgradeCapacity)

	// Capacitor.
	res.CapacitorCapacity = sg(fitAttrCapacitorCapacity)
	capRechargeSecs := sg(fitAttrCapacitorRecharge) / 1000
	if res.CapacitorCapacity > 0 && capRechargeSecs > 0 {
		res.CapacitorPeakRecharge = 2.5 * res.CapacitorCapacity / capRechargeSecs
	}
	for _, ent := range fitted {
		if ent.kind == fitEntDrone || !ent.cycling() {
			continue
		}
		need := ent.get(snap, fitAttrCapacitorNeed)
		cycle := cycleSeconds(ent, snap)
		if need > 0 && cycle > 0 {
			res.CapacitorDraw += need / cycle * float64(maxInt(ent.instances, 1))
		}
	}
	if res.CapacitorCapacity > 0 && res.CapacitorDraw > 0 {
		if res.CapacitorDraw <= res.CapacitorPeakRecharge {
			res.CapacitorStable = true
			res.CapacitorStablePercent = capacitorEquilibriumPercent(
				res.CapacitorCapacity, capRechargeSecs, res.CapacitorDraw)
		} else {
			res.CapacitorDepletionSecs = capacitorDepletionSeconds(
				res.CapacitorCapacity, capRechargeSecs, res.CapacitorDraw)
		}
	} else {
		res.CapacitorStable = true
		res.CapacitorStablePercent = 100
	}

	// Offense.
	var damageAttrs = []int64{fitAttrDamageEM, fitAttrDamageExplosive, fitAttrDamageKinetic, fitAttrDamageThermal}
	for _, ent := range fitted {
		if ent.kind != fitEntModule {
			continue
		}
		// Offline or inactive modules neither deal damage nor
		// run cap warfare.
		if !ent.cycling() {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		cycle := cycleSeconds(ent, snap)
		// Capacitor warfare drains even where damage doesn't
		// apply, so count it before the cycle gate.
		if amt := ent.get(snap, fitAttrEnergyNeutralizerAmount); amt > 0 {
			res.NeutDrainPerCycle += n * amt
			if cycle > 0 {
				res.NeutDrainPerSec += n * amt / cycle
			}
		}
		if amt := ent.get(snap, fitAttrPowerTransferAmount); amt > 0 &&
			snap.groups[ent.typeID] == fitGroupNosferatu {
			res.NosDrainPerCycle += n * amt
			if cycle > 0 {
				res.NosDrainPerSec += n * amt / cycle
			}
		}
		if cycle <= 0 {
			continue
		}
		charge := weaponCharge[ent.typeID]
		switch {
		case snap.hasEffect(ent.typeID, fitEffectTurretFitted):
			if charge == nil {
				noteUnmodeled(fmt.Sprintf("turret type %d has no charge selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= ent.get(snap, fitAttrDamageMultiplier)
			res.TurretVolley += n * volley
			res.TurretDPS += n * volley / cycle
		case snap.hasEffect(ent.typeID, fitEffectLauncherFitted):
			if charge == nil {
				noteUnmodeled(fmt.Sprintf("launcher type %d has no missile selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= charge.get(snap, fitAttrDamageMultiplier)
			volley *= charEnt.get(snap, fitAttrMissileDamageMult)
			res.MissileVolley += n * volley
			res.MissileDPS += n * volley / cycle
		}
	}

	// Drones: carried count, then active count limited by the
	// ship's bandwidth and the pilot's drone control count.
	res.DroneBandwidth = sg(fitAttrDroneBandwidth)
	res.DroneBayCapacity = sg(fitAttrDroneCapacity)
	maxActive := levels[fitAttrDronesSkill]
	var activeBudget = maxActive
	for _, ent := range drones {
		carried := ent.instances
		res.DronesFitted += carried
		if phys, ok := snap.physics[ent.typeID]; ok {
			res.DroneBayUsed += phys.Volume * float64(carried)
		}
		perDroneBW := ent.get(snap, fitAttrDroneBandwidthUsed)
		fly := carried
		if perDroneBW > 0 && res.DroneBandwidth > 0 {
			if maxByBW := int(res.DroneBandwidth / perDroneBW); fly > maxByBW {
				fly = maxByBW
			}
		}
		if fly > activeBudget {
			fly = activeBudget
		}
		if fly < 0 {
			fly = 0
		}
		activeBudget -= fly
		res.DronesActive += fly
		res.DroneBandwidthUsed += perDroneBW * float64(fly)
		cycle := cycleSeconds(ent, snap)
		if fly > 0 && cycle > 0 {
			volley := 0.0
			for _, a := range damageAttrs {
				volley += ent.get(snap, a)
			}
			volley *= ent.get(snap, fitAttrDamageMultiplier)
			res.DroneVolley += float64(fly) * volley
			res.DroneDPS += float64(fly) * volley / cycle
		}
	}
	res.DPS = res.TurretDPS + res.MissileDPS + res.DroneDPS
	res.Volley = res.TurretVolley + res.MissileVolley + res.DroneVolley

	// Tank.
	res.ShieldHP = sg(fitAttrShieldCapacity)
	res.ArmorHP = sg(fitAttrArmorHP)
	res.HullHP = sg(fitAttrHP)
	res.ShieldEHP = layerEHP(res.ShieldHP,
		sg(fitAttrShieldEM), sg(fitAttrShieldThermal), sg(fitAttrShieldKinetic), sg(fitAttrShieldExplosive))
	res.ArmorEHP = layerEHP(res.ArmorHP,
		sg(fitAttrArmorEM), sg(fitAttrArmorThermal), sg(fitAttrArmorKinetic), sg(fitAttrArmorExplosive))
	res.HullEHP = layerEHP(res.HullHP,
		sg(fitAttrResonanceEM), sg(fitAttrResonanceThermal), sg(fitAttrResonanceKinetic), sg(fitAttrResonanceExplosive))
	res.EHP = fitEHPProfile{
		Omni:      res.ShieldEHP.Omni + res.ArmorEHP.Omni + res.HullEHP.Omni,
		EM:        res.ShieldEHP.EM + res.ArmorEHP.EM + res.HullEHP.EM,
		Thermal:   res.ShieldEHP.Thermal + res.ArmorEHP.Thermal + res.HullEHP.Thermal,
		Kinetic:   res.ShieldEHP.Kinetic + res.ArmorEHP.Kinetic + res.HullEHP.Kinetic,
		Explosive: res.ShieldEHP.Explosive + res.ArmorEHP.Explosive + res.HullEHP.Explosive,
	}
	shieldRechargeSecs := sg(fitAttrShieldRechargeRate) / 1000
	if res.ShieldHP > 0 && shieldRechargeSecs > 0 {
		res.ShieldRegenPeak = 2.5 * res.ShieldHP / shieldRechargeSecs
	}
	for _, ent := range fitted {
		if ent.kind != fitEntModule || ent.instances < 1 || !ent.cycling() {
			continue
		}
		cycle := cycleSeconds(ent, snap)
		if cycle <= 0 {
			continue
		}
		n := float64(ent.instances)
		if amt := ent.get(snap, fitAttrShieldBonus); amt > 0 {
			res.ShieldBoostRate += n * amt / cycle
		}
		if amt := ent.get(snap, fitAttrArmorDamageAmount); amt > 0 {
			res.ArmorRepairRate += n * amt / cycle
		}
		if amt := ent.get(snap, fitAttrStructureDamageAmt); amt > 0 {
			res.HullRepairRate += n * amt / cycle
		}
	}

	// Mobility and targeting.
	res.Mass = sg(fitAttrMass)
	res.Velocity = sg(fitAttrMaxVelocity)
	for _, ent := range fitted {
		if ent.kind != fitEntModule || !ent.cycling() {
			continue
		}
		thrust := ent.get(snap, fitAttrSpeedBoostFactor)
		factor := ent.get(snap, fitAttrSpeedFactor)
		if thrust > 0 && factor != 0 && res.Mass > 0 {
			res.Velocity *= 1 + (factor/100)*(thrust/res.Mass)
		}
	}
	if agility := sg(fitAttrAgility); res.Mass > 0 && agility > 0 {
		res.AlignSeconds = res.Mass * agility * math.Log(4) / 1e6
	}
	res.SignatureRadius = sg(fitAttrSignatureRadius)
	res.MaxTargetRange = sg(fitAttrMaxTargetRange)
	res.ScanResolution = sg(fitAttrScanResolution)
	res.SensorStrength = math.Max(math.Max(sg(fitAttrScanRadar), sg(fitAttrScanLadar)),
		math.Max(sg(fitAttrScanMagnetometric), sg(fitAttrScanGravimetric)))
	res.MaxLockedTargets = sg(fitAttrMaxLockedTargets)
	res.CargoCapacity = sg(fitAttrCapacity)
	if res.CargoCapacity == 0 {
		if phys, ok := snap.physics[shipTypeID]; ok {
			res.CargoCapacity = phys.Capacity
		}
	}

	res.Unmodeled = make([]string, 0, len(unmodeled))
	for s := range unmodeled {
		res.Unmodeled = append(res.Unmodeled, s)
	}
	sort.Strings(res.Unmodeled)
	return res
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
func finalAttrs(snap *fitSnapshot, e *fitEntity) map[int64]float64 {
	out := cloneAttrMap(e.base)
	for attr := range e.calc {
		out[attr] = e.get(snap, attr)
	}
	return out
}

// fitRestrictionFor returns the restriction for a module on the
// given hull, or nil when the module fits. A module is
// restricted when it carries any canFitShipGroup01-04
// (1298-1301) or canFitShipType1-4 (1302-1305) attribute; it is
// allowed when the hull's type or group matches any named one.
func fitRestrictionFor(snap *fitSnapshot, shipTypeID, shipGroup, moduleTypeID int64) *fitRestriction {
	attrs := snap.attrs[moduleTypeID]
	if attrs == nil {
		return nil
	}
	r := &fitRestriction{TypeID: moduleTypeID}
	for a := int64(fitAttrCanFitShipGroup01); a <= int64(fitAttrCanFitShipGroup04); a++ {
		if g := int64(attrs[a]); g > 0 {
			r.NeedGroup = append(r.NeedGroup, g)
		}
	}
	for a := int64(fitAttrCanFitShipType1); a <= int64(fitAttrCanFitShipType4); a++ {
		if t := int64(attrs[a]); t > 0 {
			r.NeedType = append(r.NeedType, t)
		}
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

// cycleSeconds is an item's activation cycle in seconds:
// duration (73) wins, speed (51) is the weapon/drone fallback.
func cycleSeconds(e *fitEntity, snap *fitSnapshot) float64 {
	if d := e.get(snap, fitAttrDuration); d > 0 {
		return d / 1000
	}
	if s := e.get(snap, fitAttrSpeed); s > 0 {
		return s / 1000
	}
	return 0
}

// layerEHP computes a tank layer's EHP per damage type from
// hit points and resonances (EM, thermal, kinetic, explosive
// order). Omni uses the mean resonance; a resonance of 0 or
// less makes that profile immune (infinite EHP).
func layerEHP(hp float64, em, thermal, kinetic, explosive float64) fitEHPProfile {
	resist := func(r float64) float64 {
		if r <= 0 {
			return math.Inf(1)
		}
		return hp / r
	}
	mean := (em + thermal + kinetic + explosive) / 4
	return fitEHPProfile{
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
