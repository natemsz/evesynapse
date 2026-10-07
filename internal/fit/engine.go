// Package fit is the fitting simulator's stat engine: given a ship,
// what is fitted to it, the pilot's skill levels and the charges
// loaded, it works out the fit's statistics. The app's fitting pages
// are its only caller. It knows nothing about pages, sessions or ESI.
package fit

// ---------------------------------------------------------------------------
// Fitting simulator stat engine (schema 029, v0.3.21).
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
	AttrCapacitorRecharge  = 55
	AttrDamageMultiplier   = 64
	AttrShieldBonus        = 68
	AttrAgility            = 70
	AttrDuration           = 73
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
	// Cap-warfare amounts (dgmAttributeTypes): neutralizers drain
	// via energyNeutralizerAmount (97); nosferatu via
	// powerTransferAmount (90), which remote capacitor
	// transmitters (group 67) share — group 68 is the nosferatu
	// group, so the engine only counts 68 as offensive drain.
	AttrEnergyNeutralizerAmount = 97
	AttrPowerTransferAmount     = 90
	groupRemoteCapTransmitter   = 67
	groupNosferatu              = 68
)

// Slot / fitting effect IDs (dgmEffects names verified live).
const (
	EffectLoPower        = 11
	EffectHiPower        = 12
	EffectMedPower       = 13
	EffectLauncherFitted = 40
	EffectTurretFitted   = 42
	EffectRigSlot        = 2663
	EffectSubsystemSlot  = 3772
)

// Effect categories (dgmEffects): 5 is the overload/heat
// category — the one heating a module engages.
const catOverload = 5

// Module states: the values ItemInput.State carries, and a saved
// fit stores per module. pyfa/eos semantics —
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
	Effects      map[int64]*Effect // only modifier-bearing effects
	TypeEffects  map[int64][]int64 // type -> effect IDs (sorted)
	Requirements map[int64][]SkillReq
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

// ModuleValidStates lists the states a fitted module may take,
// derived from SDE data, never per-module hardcoding: rigs,
// subsystems, drones and cargo have no states; a module is
// activatable with an active-category effect — or as a weapon
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
	activatable := cats[1] ||
		snap.HasEffect(typeID, EffectTurretFitted) ||
		snap.HasEffect(typeID, EffectLauncherFitted)
	states := []string{StateOffline, StateOnline}
	if activatable {
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
	cats := snap.typeEffectCats(typeID)
	if cats[1] ||
		snap.HasEffect(typeID, EffectTurretFitted) ||
		snap.HasEffect(typeID, EffectLauncherFitted) {
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
// one entity under its module state (pyfa semantics): offline
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

		// Effects carried by the fresh types (only ones the
		// importer stored, i.e. carrying modifiers).
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
// it the fit carries (drones especially), and — for modules —
// the module state ("", online/active/offline/overheated; ""
// resolves to the type's default).
type ItemInput struct {
	TypeID   int64
	Quantity int
	State    string
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
	// StateAttrKey — so a tooltip can show an overheated
	// module's heated numbers rather than its resting ones.
	StateAttrs map[string]map[int64]float64

	// Unmodeled lists modifier shapes the engine deliberately
	// skipped (deduped, sorted): domains/funcs it cannot target,
	// overload-category effects, and similar.
	Unmodeled []string

	// Restricted lists fitted modules whose canFitShip*
	// attributes forbid the current hull. The engine flags them
	// as fit errors; it does not silently drop them.
	Restricted []Restriction
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
	state     string // module state, "" for other kinds
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

// penaltySubject reports whether this application sits in the
// stacking-penalty group for its attribute: multiplicative op,
// non-stackable attribute, fitted-module source.
func (app application) penaltySubject(snap *Snapshot) bool {
	switch app.mod.Operation {
	case opPostMult, opPostDivide, opPostPercent:
	default:
		return false
	}
	if snap.stackable(app.mod.ModifiedAttr) {
		return false
	}
	switch app.source.kind {
	case entModule, entRig, entSubsystem:
		return true
	}
	return false
}

// impact ranks a multiplicative modifier for penalty ordering:
// larger = stronger effect on the value. Multipliers are
// compared as plain ratios (pyfa eos sorts by |factor - 1|).
func impact(op int64, v float64) float64 {
	var factor float64
	switch op {
	case opPostMult:
		factor = v
	case opPostDivide:
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
	case opPostMult:
		return v
	case opPostDivide:
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
// Source attribute values are read live, so earlier stages feed
// later ones (skill pre-multipliers before percent bonuses).
// Multiplicative stages follow pyfa eos: non-penalized
// multipliers apply at full strength; penalized ones are split
// into bonus (>1) and penalty (<1) chains, each ordered by
// impact descending and damped independently. The caller passes
// the base value explicitly: fold passes always recompute from
// base, never from an earlier pass result.
func foldAttribute(snap *Snapshot, target *entity, base float64, apps []application) float64 {
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
			case opPreAssign, opPostAssign:
				value = raw
			case opPreMultiply:
				value *= raw
			case opPreDivide:
				if raw != 0 {
					value /= raw
				}
			case opModAdd:
				value += raw
			case opModSub:
				value -= raw
			}
		}
	}

	applyStage(opPreAssign)
	applyStage(opPreMultiply)
	applyStage(opPreDivide)
	applyStage(opModAdd, opModSub)

	// Multiplicative stages (PostMultiply, PostDivide,
	// PostPercent): convert to plain multipliers, apply
	// non-penalized ones fully, then the penalized chains.
	var bonuses, penalties []multEntry
	for i := range apps {
		app := &apps[i]
		op := app.mod.Operation
		if op != opPostMult && op != opPostDivide && op != opPostPercent {
			continue
		}
		v := app.source.get(snap, app.mod.ModifyingAttr)
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

	applyStage(opPostAssign)
	return value
}

// multEntry is one penalty-subject multiplicative modifier
// reduced to a plain multiplier plus its ordering impact.
type multEntry struct {
	factor float64
	impact float64
}

// Compute runs the stat engine over one fit. charges maps a
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
func Compute(snap *Snapshot, shipTypeID int64, items []ItemInput, levels map[int64]int, charges map[int64]int64, implants []int64) *Result {
	res := &Result{
		ShipTypeID: shipTypeID,
		ItemAttrs:  make(map[int64]map[int64]float64),
		StateAttrs: make(map[string]map[int64]float64),
	}
	unmodeled := make(map[string]bool)
	noteUnmodeled := func(s string) { unmodeled[s] = true }

	// --- Entities -------------------------------------------------
	shipBase := cloneAttrMap(snap.Attrs[shipTypeID])
	if phys, ok := snap.Physics[shipTypeID]; ok && phys.Mass > 0 {
		if _, has := shipBase[AttrMass]; !has {
			shipBase[AttrMass] = phys.Mass
		}
	}
	ship := &entity{key: "ship", kind: entShip, typeID: shipTypeID, instances: 1, base: shipBase, calc: map[int64]float64{}}
	charEnt := &entity{key: "char", kind: entChar, base: map[int64]float64{}, calc: map[int64]float64{}}

	entities := []*entity{ship, charEnt}
	byType := map[int64]*entity{shipTypeID: ship}
	skillEnts := make(map[int64]*entity)

	// Module/rig/drone entities per fitted type (instances fold
	// duplicates of the same type into one entity with a count).
	// Modules fold by (type, state): two identical modules in
	// different states are separate entities so each binds only
	// the effect categories its state allows.
	var fitted []*entity // modules, rigs, subsystems, drones
	fittedByType := make(map[int64][]*entity)
	for _, it := range items {
		if it.TypeID == shipTypeID {
			continue
		}
		qty := it.Quantity
		kind := entModule
		switch {
		case snap.HasEffect(it.TypeID, EffectRigSlot):
			kind = entRig
		case snap.HasEffect(it.TypeID, EffectSubsystemSlot):
			kind = entSubsystem
		case snap.Attrs[it.TypeID][AttrDroneBandwidthUsed] > 0:
			kind = entDrone
		}
		if qty <= 0 {
			qty = 1
			if kind == entDrone {
				qty = 0 // a drone line with no count carries nothing
			}
		}
		state := ""
		if kind == entModule {
			state = NormalizeModuleState(snap, it.TypeID, it.State)
		}
		var ent *entity
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
		ent = &entity{
			key:       fmt.Sprintf("fit:%d:%s", it.TypeID, state),
			kind:      kind,
			typeID:    it.TypeID,
			instances: qty,
			state:     state,
			base:      cloneAttrMap(snap.Attrs[it.TypeID]),
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
	weaponCharge := make(map[int64]*entity) // weapon type -> charge/missile entity
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
		kind := entCharge
		if snap.HasEffect(weaponType, EffectLauncherFitted) {
			kind = entMissile
		}
		ent := &entity{
			key:       fmt.Sprintf("charge:%d", chargeType),
			kind:      kind,
			typeID:    chargeType,
			instances: 1,
			base:      cloneAttrMap(snap.Attrs[chargeType]),
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
		base := cloneAttrMap(snap.Attrs[skillID])
		base[AttrSkillLevel] = float64(lvl)
		ent := &entity{
			key:       fmt.Sprintf("skill:%d", skillID),
			kind:      entSkill,
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
	var implantEnts []*entity
	seenImplant := make(map[int64]bool)
	for _, implantID := range implants {
		if implantID <= 0 || implantID == shipTypeID || seenImplant[implantID] {
			continue
		}
		seenImplant[implantID] = true
		ent := &entity{
			key:       fmt.Sprintf("implant:%d", implantID),
			kind:      entImplant,
			typeID:    implantID,
			instances: 1,
			base:      cloneAttrMap(snap.Attrs[implantID]),
			calc:      map[int64]float64{},
		}
		implantEnts = append(implantEnts, ent)
		entities = append(entities, ent)
		byType[implantID] = ent
	}

	// --- Ship restrictions -----------------------------------------
	// Modules carrying canFitShipGroup*/canFitShipType* may only
	// fly on the named hulls. Flag violations as fit errors.
	shipGroup := snap.Groups[shipTypeID]
	for _, ent := range fitted {
		if r := restrictionFor(snap, shipTypeID, shipGroup, ent.typeID); r != nil {
			res.Restricted = append(res.Restricted, *r)
		}
	}

	// --- Modifier binding ------------------------------------------
	// atShip are the entities sitting at the ship's location for
	// Location* selectors: fitted equipment plus launched-craft
	// types (drones, missiles).
	atShip := make([]*entity, 0, len(fitted)+len(weaponCharge))
	atShip = append(atShip, fitted...)
	for _, ent := range weaponCharge {
		atShip = append(atShip, ent)
	}
	var drones []*entity
	for _, ent := range fitted {
		if ent.kind == entDrone {
			drones = append(drones, ent)
		}
	}

	var apps []application
	bind := func(source *entity, effectID int64, eff *Effect, m Modifier) {
		if m.ModifiedAttr == AttrSkillLevel {
			return // skill levels are engine inputs
		}
		targets := func(list ...*entity) {
			for _, t := range list {
				if t != nil {
					apps = append(apps, application{source: source, target: t, effectID: effectID, mod: m})
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
				if source.kind == entCharge || source.kind == entMissile {
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
					case entModule, entRig, entSubsystem:
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
					if t.kind == entCharge {
						continue // contained in its weapon, not at the ship
					}
					if snap.Groups[t.typeID] == m.GroupID && m.GroupID != 0 {
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
					if snap.Groups[t.typeID] == m.GroupID && m.GroupID != 0 {
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
				if t.kind == entCharge {
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
	sources := make([]*entity, 0, len(entities))
	sources = append(sources, ship)
	skillIDs := make([]int64, 0, len(skillEnts))
	for id := range skillEnts {
		skillIDs = append(skillIDs, id)
	}
	sort.Slice(skillIDs, func(i, j int) bool { return skillIDs[i] < skillIDs[j] })
	for _, id := range skillIDs {
		sources = append(sources, skillEnts[id])
	}
	sources = append(sources, implantEnts...)
	sources = append(sources, fitted...)
	for _, ent := range weaponCharge {
		sources = append(sources, ent)
	}

	for _, source := range sources {
		// Effect categories bind per module state (pyfa
		// semantics): an offline module binds nothing, an
		// online-but-inactive one skips active and overload,
		// overheated adds the overload category.
		allowed := sourceCategories(source)
		if allowed == nil {
			continue
		}
		for _, effectID := range snap.TypeEffects[source.typeID] {
			eff := snap.Effects[effectID]
			if eff == nil {
				continue // carries no modifiers; nothing to apply
			}
			if !allowed[eff.Category] {
				// A heatable module that simply isn't heated is
				// a fitting choice, not a modeling gap — skip
				// silently so the heat note only fires for
				// genuinely unmodeled categories.
				if eff.Category == catOverload && source.kind == entModule {
					continue
				}
				noteUnmodeled(fmt.Sprintf("effect %d %s: category %d skipped (overload/system/target mechanics not modeled)", effectID, eff.Name, eff.Category))
				continue
			}
			for _, m := range eff.Modifiers {
				// Instances of the same fitted type bind once per
				// instance so stacking penalties see each copy.
				n := 1
				if source.kind == entModule || source.kind == entRig || source.kind == entSubsystem {
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
		target *entity
		attr   int64
	}
	grouped := make(map[appKey][]application)
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
		if ent == ship || ent.kind == entSkill || ent == charEnt {
			continue
		}
		fa := finalAttrs(snap, ent)
		if _, ok := res.ItemAttrs[ent.typeID]; !ok {
			res.ItemAttrs[ent.typeID] = fa
		}
		if ent.kind == entModule && ent.state != "" {
			res.StateAttrs[StateAttrKey(ent.typeID, ent.state)] = fa
		}
	}

	// --- Derived stats ----------------------------------------------
	sg := func(attr int64) float64 { return ship.get(snap, attr) }

	// Resources. Offline modules are fitted but dark: no PG,
	// no CPU, no effects.
	for _, ent := range fitted {
		if ent.kind == entDrone || !ent.online() {
			continue
		}
		res.PowergridUsed += ent.get(snap, AttrPower) * float64(maxInt(ent.instances, 1))
		res.CPUUsed += ent.get(snap, AttrCPU) * float64(maxInt(ent.instances, 1))
	}
	res.PowergridMax = sg(AttrPowerOutput)
	res.CPUMax = sg(AttrCPUOutput)

	// Slots and hardpoints.
	res.HighSlots = int(sg(AttrHiSlots))
	res.MediumSlots = int(sg(AttrMedSlots))
	res.LowSlots = int(sg(AttrLowSlots))
	res.RigSlots = int(sg(AttrRigSlots))
	res.TurretHardpoints = int(sg(AttrTurretSlotsLeft))
	res.LauncherHardpoints = int(sg(AttrLauncherSlotsLeft))
	// T3 strategic cruisers: the hull has no slots of its own;
	// fitted subsystems grant them through the *SlotModifier
	// attributes above (plain data, summed directly).
	for _, ent := range fitted {
		if ent.kind != entSubsystem {
			continue
		}
		n := float64(maxInt(ent.instances, 1))
		res.HighSlots += int(ent.base[AttrHiSlotModifier] * n)
		res.MediumSlots += int(ent.base[AttrMedSlotModifier] * n)
		res.LowSlots += int(ent.base[AttrLowSlotModifier] * n)
		res.TurretHardpoints += int(ent.base[AttrTurretHardPointModifier] * n)
		res.LauncherHardpoints += int(ent.base[AttrLauncherHardPointModifier] * n)
	}
	for _, ent := range fitted {
		n := maxInt(ent.instances, 1)
		switch {
		case ent.kind == entRig:
			res.RigSlotsUsed += n
			res.CalibrationUsed += ent.get(snap, AttrUpgradeCost) * float64(n)
		case ent.kind == entModule:
			switch {
			case snap.HasEffect(ent.typeID, EffectHiPower):
				res.HighSlotsUsed += n
			case snap.HasEffect(ent.typeID, EffectMedPower):
				res.MediumSlotsUsed += n
			case snap.HasEffect(ent.typeID, EffectLoPower):
				res.LowSlotsUsed += n
			}
			if snap.HasEffect(ent.typeID, EffectTurretFitted) {
				res.TurretHardpointsUsed += n
			}
			if snap.HasEffect(ent.typeID, EffectLauncherFitted) {
				res.LauncherHardpointsUsed += n
			}
		}
	}
	res.CalibrationMax = sg(AttrUpgradeCapacity)

	// Capacitor.
	res.CapacitorCapacity = sg(AttrCapacitorCapacity)
	capRechargeSecs := sg(AttrCapacitorRecharge) / 1000
	if res.CapacitorCapacity > 0 && capRechargeSecs > 0 {
		res.CapacitorPeakRecharge = 2.5 * res.CapacitorCapacity / capRechargeSecs
	}
	for _, ent := range fitted {
		if ent.kind == entDrone || !ent.cycling() {
			continue
		}
		need := ent.get(snap, AttrCapacitorNeed)
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
	var damageAttrs = []int64{AttrDamageEM, AttrDamageExplosive, AttrDamageKinetic, AttrDamageThermal}
	for _, ent := range fitted {
		if ent.kind != entModule {
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
		if amt := ent.get(snap, AttrEnergyNeutralizerAmount); amt > 0 {
			res.NeutDrainPerCycle += n * amt
			if cycle > 0 {
				res.NeutDrainPerSec += n * amt / cycle
			}
		}
		if amt := ent.get(snap, AttrPowerTransferAmount); amt > 0 &&
			snap.Groups[ent.typeID] == groupNosferatu {
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
		case snap.HasEffect(ent.typeID, EffectTurretFitted):
			if charge == nil {
				noteUnmodeled(fmt.Sprintf("turret type %d has no charge selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= ent.get(snap, AttrDamageMultiplier)
			res.TurretVolley += n * volley
			res.TurretDPS += n * volley / cycle
		case snap.HasEffect(ent.typeID, EffectLauncherFitted):
			if charge == nil {
				noteUnmodeled(fmt.Sprintf("launcher type %d has no missile selected; damage not counted", ent.typeID))
				continue
			}
			volley := 0.0
			for _, a := range damageAttrs {
				volley += charge.get(snap, a)
			}
			volley *= charge.get(snap, AttrDamageMultiplier)
			volley *= charEnt.get(snap, AttrMissileDamageMult)
			res.MissileVolley += n * volley
			res.MissileDPS += n * volley / cycle
		}
	}

	// Drones: carried count, then active count limited by the
	// ship's bandwidth and the pilot's drone control count.
	res.DroneBandwidth = sg(AttrDroneBandwidth)
	res.DroneBayCapacity = sg(AttrDroneCapacity)
	maxActive := levels[AttrDronesSkill]
	var activeBudget = maxActive
	for _, ent := range drones {
		carried := ent.instances
		res.DronesFitted += carried
		if phys, ok := snap.Physics[ent.typeID]; ok {
			res.DroneBayUsed += phys.Volume * float64(carried)
		}
		perDroneBW := ent.get(snap, AttrDroneBandwidthUsed)
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
			volley *= ent.get(snap, AttrDamageMultiplier)
			res.DroneVolley += float64(fly) * volley
			res.DroneDPS += float64(fly) * volley / cycle
		}
	}
	res.DPS = res.TurretDPS + res.MissileDPS + res.DroneDPS
	res.Volley = res.TurretVolley + res.MissileVolley + res.DroneVolley

	// Tank.
	res.ShieldHP = sg(AttrShieldCapacity)
	res.ArmorHP = sg(AttrArmorHP)
	res.HullHP = sg(AttrHP)
	res.ShieldEHP = layerEHP(res.ShieldHP,
		sg(AttrShieldEM), sg(AttrShieldThermal), sg(AttrShieldKinetic), sg(AttrShieldExplosive))
	res.ArmorEHP = layerEHP(res.ArmorHP,
		sg(AttrArmorEM), sg(AttrArmorThermal), sg(AttrArmorKinetic), sg(AttrArmorExplosive))
	res.HullEHP = layerEHP(res.HullHP,
		sg(AttrResonanceEM), sg(AttrResonanceThermal), sg(AttrResonanceKinetic), sg(AttrResonanceExplosive))
	res.EHP = EHPProfile{
		Omni:      res.ShieldEHP.Omni + res.ArmorEHP.Omni + res.HullEHP.Omni,
		EM:        res.ShieldEHP.EM + res.ArmorEHP.EM + res.HullEHP.EM,
		Thermal:   res.ShieldEHP.Thermal + res.ArmorEHP.Thermal + res.HullEHP.Thermal,
		Kinetic:   res.ShieldEHP.Kinetic + res.ArmorEHP.Kinetic + res.HullEHP.Kinetic,
		Explosive: res.ShieldEHP.Explosive + res.ArmorEHP.Explosive + res.HullEHP.Explosive,
	}
	shieldRechargeSecs := sg(AttrShieldRechargeRate) / 1000
	if res.ShieldHP > 0 && shieldRechargeSecs > 0 {
		res.ShieldRegenPeak = 2.5 * res.ShieldHP / shieldRechargeSecs
	}
	for _, ent := range fitted {
		if ent.kind != entModule || ent.instances < 1 || !ent.cycling() {
			continue
		}
		cycle := cycleSeconds(ent, snap)
		if cycle <= 0 {
			continue
		}
		n := float64(ent.instances)
		if amt := ent.get(snap, AttrShieldBonus); amt > 0 {
			res.ShieldBoostRate += n * amt / cycle
		}
		if amt := ent.get(snap, AttrArmorDamageAmount); amt > 0 {
			res.ArmorRepairRate += n * amt / cycle
		}
		if amt := ent.get(snap, AttrStructureDamageAmt); amt > 0 {
			res.HullRepairRate += n * amt / cycle
		}
	}

	// Mobility and targeting.
	res.Mass = sg(AttrMass)
	res.Velocity = sg(AttrMaxVelocity)
	for _, ent := range fitted {
		if ent.kind != entModule || !ent.cycling() {
			continue
		}
		thrust := ent.get(snap, AttrSpeedBoostFactor)
		factor := ent.get(snap, AttrSpeedFactor)
		if thrust > 0 && factor != 0 && res.Mass > 0 {
			res.Velocity *= 1 + (factor/100)*(thrust/res.Mass)
		}
	}
	if agility := sg(AttrAgility); res.Mass > 0 && agility > 0 {
		res.AlignSeconds = res.Mass * agility * math.Log(4) / 1e6
	}
	res.SignatureRadius = sg(AttrSignatureRadius)
	res.MaxTargetRange = sg(AttrMaxTargetRange)
	res.ScanResolution = sg(AttrScanResolution)
	res.SensorStrength = math.Max(math.Max(sg(AttrScanRadar), sg(AttrScanLadar)),
		math.Max(sg(AttrScanMagnetometric), sg(AttrScanGravimetric)))
	res.MaxLockedTargets = sg(AttrMaxLockedTargets)
	res.CargoCapacity = sg(AttrCapacity)
	if res.CargoCapacity == 0 {
		if phys, ok := snap.Physics[shipTypeID]; ok {
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
func finalAttrs(snap *Snapshot, e *entity) map[int64]float64 {
	out := cloneAttrMap(e.base)
	for attr := range e.calc {
		out[attr] = e.get(snap, attr)
	}
	return out
}

// restrictionFor returns the restriction for a module on the
// given hull, or nil when the module fits. A module is
// restricted when it carries any canFitShipGroup01-04
// (1298-1301) or canFitShipType1-4 (1302-1305) attribute; it is
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
func cycleSeconds(e *entity, snap *Snapshot) float64 {
	if d := e.get(snap, AttrDuration); d > 0 {
		return d / 1000
	}
	if s := e.get(snap, AttrSpeed); s > 0 {
		return s / 1000
	}
	return 0
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
