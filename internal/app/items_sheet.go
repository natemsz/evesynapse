package app

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"evesynapse/internal/app/module"
	"evesynapse/internal/esi"
	"evesynapse/internal/fitview"
	"evesynapse/internal/humanize"
	"evesynapse/internal/itemsheet"
	"evesynapse/internal/logging"
)

// itemSheetView is a type's stats as the item page shows them, built
// from its stored item sheet.
type itemSheetView struct {
	IsShip       bool
	Slots        []itemSlotTile
	Bonuses      []itemsheet.BonusBlock
	Resists      []itemResistRow
	Sections     []itemsheet.Section
	FitsTo       []itemFitsGroup
	Skills       []itemSkillLine
	SkillPilot   string // the signed-in pilot the marks are for; "" when none
	WorkbenchURL string
	Variations   *itemVariationsView
}

// itemSlotTile is one ship slot count on the slot strip.
type itemSlotTile struct {
	Label string
	Value string
}

// itemResistRow is one defense layer's hit points and four
// resistances, drawn with the workbench's resist cells.
type itemResistRow struct {
	Layer string
	HP    string
	Cells []itemResistCell
}

type itemResistCell struct {
	Kind string // em, th, kin, exp: the workbench's res-* classes
	Pct  float64
	Text string
}

// itemFitsGroup is one line of "Fits to": the slot it takes, the
// charges or launchers it is used with, or the hulls it is limited to.
type itemFitsGroup struct {
	Label string
	Tags  []itemFitsTag
}

type itemFitsTag struct {
	Label string
	Link  string
}

// itemSkillLine is one required skill with the signed-in pilot's
// mark: Mark is "ok" when trained to the level, "short" when not,
// "" when no pilot's skills are known.
type itemSkillLine struct {
	SkillID int64
	Name    string
	Level   int64
	Depth   int
	Mark    string
	Trained int
}

// itemVariationsView is the compare table of a module's variation
// family, with our own price on each row.
type itemVariationsView struct {
	Columns []string
	Rows    []itemVariationRow
}

type itemVariationRow struct {
	TypeID    int64
	Name      string
	Tier      string
	Values    []string
	Price     string // "" when no tier can price it
	PriceNote string // the tier's provenance and age when not a fresh hub price
	Current   bool
}

// Ship attributes the sheet view moves out of the attribute list: the
// slot strip and the resist grid show them instead.
var (
	shipSlotAttrs = []struct {
		id    int64
		label string
	}{
		{14, "High slots"}, {13, "Mid slots"}, {12, "Low slots"}, {1154, "Rig slots"},
		{102, "Turrets"}, {101, "Launchers"}, {283, "Drone bay"}, {1271, "Bandwidth"},
	}
	resistLayers = []struct {
		layer string
		hp    int64
		ids   [4]int64 // em, thermal, kinetic, explosive resonance
	}{
		{"Shield", 263, [4]int64{271, 274, 273, 272}},
		{"Armor", 265, [4]int64{267, 270, 269, 268}},
		{"Hull", 9, [4]int64{113, 110, 109, 111}},
	}
	resistKinds = [4]string{"em", "th", "kin", "exp"}
)

// Slot effects, from the dump's dgmEffects: which rack a module takes.
var slotEffects = map[int64]string{11: "Low slot", 12: "High slot", 13: "Mid slot", 2663: "Rig slot", 3772: "Subsystem slot"}

// buildItemSheetView reads one type's sheet, and for a module its
// variation table. categoryID is the type's category.
func (app *Application) buildItemSheetView(ctx context.Context, typeID, categoryID int64) *itemSheetView {
	sh, err := itemsheet.Load(ctx, app.queries, typeID)
	if err != nil {
		if !errors.Is(err, itemsheet.ErrNotFound) {
			logging.Errorf("items: sheet %d: %v", typeID, err)
		}
		return nil
	}
	v := &itemSheetView{IsShip: categoryID == itemsheet.CategoryShip, Bonuses: sh.Bonuses}
	moved := map[int64]bool{}

	if v.IsShip {
		v.WorkbenchURL = "/fittings/?ship=" + strconv.FormatInt(typeID, 10) + "#fit-editor"
		for _, s := range shipSlotAttrs {
			val, ok := sh.Value(s.id)
			if !ok {
				continue
			}
			moved[s.id] = true
			text := itemsheet.Number(val)
			switch s.id {
			case 283:
				text += " m3"
			case 1271:
				text += " Mbit/s"
			}
			v.Slots = append(v.Slots, itemSlotTile{Label: s.label, Value: text})
		}
		for _, l := range resistLayers {
			row := itemResistRow{Layer: l.layer}
			if hp, ok := sh.Value(l.hp); ok {
				row.HP = itemsheet.Number(hp) + " HP"
				moved[l.hp] = true
			}
			for i, id := range l.ids {
				res, ok := sh.Value(id)
				if !ok {
					res = 1 // no resistance
				}
				moved[id] = true
				pct := math.Max(0, math.Min(100, (1-res)*100))
				row.Cells = append(row.Cells, itemResistCell{Kind: resistKinds[i], Pct: pct, Text: itemsheet.Number(math.Round(pct*10)/10) + "%"})
			}
			v.Resists = append(v.Resists, row)
		}
	}

	// Fits to: the rack, then what it is used with, then the hulls
	// it is limited to.
	if eff, err := app.queries.ListSDETypeEffects(ctx, typeID); err == nil {
		for _, e := range eff {
			if slot, ok := slotEffects[e.EffectID]; ok {
				v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Slot", Tags: []itemFitsTag{{Label: slot}}})
				break
			}
		}
	}
	var uses, hulls []itemFitsTag
	for _, sec := range sh.Sections {
		for _, r := range sec.Rows {
			switch {
			case strings.HasPrefix(r.Name, "chargeGroup"), strings.HasPrefix(r.Name, "launcherGroup"):
				uses = append(uses, itemFitsTag{Label: r.Value, Link: r.Link})
				moved[r.AttributeID] = true
			case strings.HasPrefix(r.Name, "canFitShip"):
				hulls = append(hulls, itemFitsTag{Label: r.Value, Link: r.Link})
				moved[r.AttributeID] = true
			}
		}
	}
	if len(uses) > 0 {
		v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Used with", Tags: uses})
	}
	if len(hulls) > 0 {
		v.FitsTo = append(v.FitsTo, itemFitsGroup{Label: "Only fits on", Tags: hulls})
	}

	for _, sec := range sh.Sections {
		kept := itemsheet.Section{Title: sec.Title}
		for _, r := range sec.Rows {
			if !moved[r.AttributeID] {
				kept.Rows = append(kept.Rows, r)
			}
		}
		if len(kept.Rows) > 0 {
			v.Sections = append(v.Sections, kept)
		}
	}

	app.attachSheetSkills(ctx, v, sh.Skills)
	if !v.IsShip {
		v.Variations = app.itemVariations(ctx, typeID)
	}
	return v
}

// attachSheetSkills copies the skill tree and, for a signed-in
// account, marks each line against the acting pilot's stored skills.
func (app *Application) attachSheetSkills(ctx context.Context, v *itemSheetView, lines []itemsheet.SkillLine) {
	var levels map[int64]int
	if chars := app.sessionCharacters(ctx); len(chars) > 0 {
		pilot := app.actingCharacterID(ctx, chars)
		var skills esi.Skills
		if pilot != 0 && app.loadCorpSnapshot(ctx, pilot, esi.SnapSkills, &skills) {
			levels = fitview.ActiveSkillLevels(skills)
			for _, ch := range chars {
				if ch.CharacterID == pilot {
					v.SkillPilot = ch.Name
				}
			}
		}
	}
	for _, l := range lines {
		line := itemSkillLine{SkillID: l.SkillID, Name: l.Name, Level: l.Level, Depth: l.Depth}
		if levels != nil {
			line.Trained = levels[l.SkillID]
			line.Mark = "short"
			if int64(line.Trained) >= l.Level {
				line.Mark = "ok"
			}
		}
		v.Skills = append(v.Skills, line)
	}
}

// itemVariations reads the stored compare table of the type's
// variation family and prices each member in the house order (own
// swept orders, own estimates, the labeled outside fallback, else no
// price). Nil when the type has no family.
func (app *Application) itemVariations(ctx context.Context, typeID int64) *itemVariationsView {
	rows, err := app.queries.ListItemVariationRows(ctx, typeID)
	if err != nil {
		logging.Errorf("items: variations %d: %v", typeID, err)
		return nil
	}
	if len(rows) < 2 {
		return nil
	}
	cols, err := app.queries.ListItemVariationColumns(ctx, typeID)
	if err != nil {
		logging.Errorf("items: variation columns %d: %v", typeID, err)
		return nil
	}
	view := &itemVariationsView{}
	for _, c := range cols {
		view.Columns = append(view.Columns, c.DisplayName)
	}
	lines := make([]module.AppraisalLine, 0, len(rows))
	for _, r := range rows {
		lines = append(lines, module.AppraisalLine{TypeID: r.TypeID, Quantity: 1})
	}
	prices := map[int64]module.AppraisedLine{}
	if res, err := app.AppraiseLines(ctx, lines); err == nil {
		for _, l := range res.Lines {
			prices[l.TypeID] = l
		}
	} else {
		logging.Errorf("items: variation prices %d: %v", typeID, err)
	}
	now := time.Now()
	for _, r := range rows {
		row := itemVariationRow{TypeID: r.TypeID, Name: r.Name, Tier: r.MetaGroupName, Current: r.TypeID == typeID}
		for i, c := range cols {
			if i >= len(r.Vals) || math.IsNaN(r.Vals[i]) {
				row.Values = append(row.Values, "")
				continue
			}
			row.Values = append(row.Values, itemsheet.FormatValue(c.UnitID, c.UnitName, r.Vals[i]))
		}
		if p, ok := prices[r.TypeID]; ok && p.Priced {
			row.Price = esi.FormatISK(p.UnitPrice)
			row.PriceNote = variationPriceNote(p, now)
		}
		view.Rows = append(view.Rows, row)
	}
	return view
}

// variationPriceNote names where a price came from when it is not a
// fresh hub price, with its age.
func variationPriceNote(p module.AppraisedLine, now time.Time) string {
	age := ""
	if !p.AsOf.IsZero() {
		age = humanize.Duration(now.Sub(p.AsOf)) + " old"
	}
	switch p.Tier {
	case module.AppraisalTierTypical:
		if now.Sub(p.AsOf) >= fallbackOwnFresh {
			return "Forge price, " + age
		}
		return ""
	case module.AppraisalTierEstimate:
		return "Estimate"
	case module.AppraisalTierFallback:
		return "Fuzzwork, " + age
	}
	return ""
}
