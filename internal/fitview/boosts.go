package fitview

import (
	"context"
	"fmt"
	"math"
	"strings"

	"evesynapse/internal/fit"
	"evesynapse/internal/logging"
)

// FitBoost is one fleet boost acting on the fit: a booster's burst
// charge, the booster's hull (0: no hull bonus), its skill level for
// every skill, and its mindlink (0: none).
type FitBoost struct {
	Charge   int64 `json:"charge"`
	Hull     int64 `json:"hull,omitempty"`
	Level    int   `json:"level,omitempty"`
	Mindlink int64 `json:"mindlink,omitempty"`
}

// FitMaxBoosts bounds a fit document's fleet boosts: one of every
// burst charge across the families is far more than a fleet runs.
const FitMaxBoosts = 12

// sanitizeBoosts bounds a document's boosts: positive charge types,
// at most FitMaxBoosts, levels 1 to 5 (unset means V).
func sanitizeBoosts(doc *FitDoc) {
	out := doc.Boosts[:0]
	for _, b := range doc.Boosts {
		if b.Charge <= 0 || len(out) >= FitMaxBoosts {
			continue
		}
		if b.Level < 1 || b.Level > 5 {
			b.Level = 5
		}
		if b.Hull < 0 {
			b.Hull = 0
		}
		if b.Mindlink < 0 {
			b.Mindlink = 0
		}
		out = append(out, b)
	}
	doc.Boosts = out
}

// FitBoostTypeIDs are the types the document's boosts need in the
// snapshot: each charge, booster hull, mindlink and the burst module
// that loads the charge.
func FitBoostTypeIDs(doc *FitDoc, bursts *fit.BurstData) []int64 {
	var ids []int64
	for _, b := range doc.Boosts {
		ids = append(ids, b.Charge)
		if b.Hull > 0 {
			ids = append(ids, b.Hull)
		}
		if b.Mindlink > 0 {
			ids = append(ids, b.Mindlink)
		}
		if bursts != nil {
			for _, c := range bursts.Charges {
				if c.TypeID == b.Charge {
					ids = append(ids, bursts.ModuleFor[c.Group])
				}
			}
		}
	}
	return ids
}

// FitBoostRow is one fleet boost as the workbench shows it: its
// choices, and what it does.
type FitBoostRow struct {
	Index     int
	Families  []FitBoostFamily // the charge picker, by burst family
	Hulls     []FitBoostOpt
	Mindlinks []FitBoostOpt
	Levels    []FitBoostOpt
	Label     string // charge name, for the read-only view
	Effects   string // what the booster's burst gives, in words
}

// FitBoostFamily is one burst family's charges.
type FitBoostFamily struct {
	Name    string
	Charges []FitBoostOpt
}

// FitBoostOpt is one choice in a boost picker.
type FitBoostOpt struct {
	ID       int64
	Name     string
	Selected bool
}

// FitFleetBoosts checks the document's boosts against the burst data
// (an unknown charge is dropped; a hull or mindlink that does not
// bonus the charge's family falls back to none), works out each
// booster's buffs, and builds the picker rows. skillIDs is every
// skill: a booster's level applies to all of them.
func FitFleetBoosts(snap *fit.Snapshot, doc *FitDoc, skillIDs []int64) ([]fit.BuffValue, []FitBoostRow) {
	bursts := snap.Bursts
	if bursts == nil || len(doc.Boosts) == 0 {
		return nil, nil
	}
	chargeOf := map[int64]fit.BurstCharge{}
	for _, c := range bursts.Charges {
		chargeOf[c.TypeID] = c
	}
	var values []fit.BuffValue
	var rows []FitBoostRow
	kept := doc.Boosts[:0]
	for _, b := range doc.Boosts {
		c, ok := chargeOf[b.Charge]
		if !ok {
			continue
		}
		row := FitBoostRow{Index: len(kept), Label: c.Name}
		hullOK, mindOK := false, false
		row.Hulls = append(row.Hulls, FitBoostOpt{Name: "No hull bonus"})
		row.Mindlinks = append(row.Mindlinks, FitBoostOpt{Name: "No mindlink"})
		for _, bo := range bursts.Boosters {
			if bo.Skill != c.Skill {
				continue
			}
			opt := FitBoostOpt{ID: bo.TypeID, Name: bo.Name}
			switch bo.Category {
			case 6:
				opt.Selected = bo.TypeID == b.Hull
				hullOK = hullOK || opt.Selected
				row.Hulls = append(row.Hulls, opt)
			case 20:
				opt.Selected = bo.TypeID == b.Mindlink
				mindOK = mindOK || opt.Selected
				row.Mindlinks = append(row.Mindlinks, opt)
			}
		}
		if !hullOK {
			b.Hull = 0
			row.Hulls[0].Selected = true
		}
		if !mindOK {
			b.Mindlink = 0
			row.Mindlinks[0].Selected = true
		}
		for lv := 1; lv <= 5; lv++ {
			row.Levels = append(row.Levels, FitBoostOpt{ID: int64(lv), Name: fmt.Sprintf("Skills at %s", romanLevel(lv)), Selected: lv == b.Level})
		}
		var fam *FitBoostFamily
		for _, cc := range bursts.Charges {
			if fam == nil || fam.Name != cc.Family {
				row.Families = append(row.Families, FitBoostFamily{Name: cc.Family})
				fam = &row.Families[len(row.Families)-1]
			}
			fam.Charges = append(fam.Charges, FitBoostOpt{ID: cc.TypeID, Name: cc.Name, Selected: cc.TypeID == b.Charge})
		}

		levels := make(map[int64]int, len(skillIDs))
		for _, id := range skillIDs {
			levels[id] = b.Level
		}
		var implants []int64
		if b.Mindlink > 0 {
			implants = []int64{b.Mindlink}
		}
		got := fit.BoosterBuffs(snap, b.Hull, b.Charge, levels, implants)
		values = append(values, got...)
		row.Effects = FitBuffWords(bursts, got)
		rows = append(rows, row)
		kept = append(kept, b)
	}
	doc.Boosts = kept
	return values, rows
}

// FitBuffWords says what buffs do: "Armor Resistance Bonus -21.6%".
func FitBuffWords(bursts *fit.BurstData, buffs []fit.BuffValue) string {
	var out strings.Builder
	for i, v := range buffs {
		if i > 0 {
			out.WriteString(", ")
		}
		out.WriteString(FitBuffWord(bursts, v.ID, v.Value))
	}
	return out.String()
}

// FitBuffWord is one buff in words, its value shown the way its
// operation reads.
func FitBuffWord(bursts *fit.BurstData, id int64, value float64) string {
	name := fmt.Sprintf("Buff %d", id)
	op := int64(6)
	if b := bursts.Buffs[id]; b != nil {
		if b.Name != "" {
			name = b.Name
		}
		op = b.Operation
	}
	switch op {
	case 6: // percent
		return fmt.Sprintf("%s %s%%", name, signed1(value))
	case 2: // add
		return fmt.Sprintf("%s %s", name, signed1(value))
	case 4: // multiply
		return fmt.Sprintf("%s ×%s", name, FitFmt1(value))
	}
	return fmt.Sprintf("%s %s", name, FitFmt1(value))
}

func signed1(v float64) string {
	if v > 0 {
		return "+" + FitFmt1(v)
	}
	return "−" + FitFmt1(math.Abs(v))
}

func romanLevel(n int) string {
	return [...]string{"", "I", "II", "III", "IV", "V"}[n]
}

// fitAllSkillIDs is every skill's type id, for a booster's levels.
func (app *Builder) fitAllSkillIDs(ctx context.Context, snap *fit.Snapshot) []int64 {
	ids, err := fit.WithAllSkills(ctx, app.queries, snap)
	if err != nil {
		logging.Errorf("fittings sim: load skill dogma for boosters: %v", err)
	}
	return ids
}

// FitDocBoosts is the fleet boosts a fit gives when it flies as a
// booster: every burst charge it loads, once, from its hull with its
// slot 10 implant as the mindlink, at skills V. FitFleetBoosts drops a
// hull or mindlink that does not raise the charge's family.
func FitDocBoosts(doc *FitDoc, bursts *fit.BurstData) []FitBoost {
	if doc == nil || bursts == nil {
		return nil
	}
	isBurst := map[int64]bool{}
	for _, c := range bursts.Charges {
		isBurst[c.TypeID] = true
	}
	var out []FitBoost
	for _, c := range doc.LoadedCharges() {
		if !isBurst[c] || len(out) >= FitMaxBoosts {
			continue
		}
		out = append(out, FitBoost{Charge: c, Hull: doc.ShipTypeID, Level: 5, Mindlink: doc.Implants[FitImplantSlots]})
	}
	return out
}
